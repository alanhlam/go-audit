package main

import (
	"bytes"
	"encoding/hex"
	"net"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	SYSCALL           = 1300            // Syscall event
	CONFIG_CHANGE     = 1305            // Audit system configuration change
	SOCKADDR          = 1306            // Sockaddr copied as syscall arg
	CWD               = 1307            // Current working directory
	EXECVE            = 1309            // Execve arguments
	HEADER_MIN_LENGTH = 7               // Minimum length of an audit header
	HEADER_START_POS  = 6               // Position in the audit header that the data starts
	COMPLETE_AFTER    = time.Second * 2 // Log a message after this time or EOE
	SOCKADDR_LENGTH   = 34              // Length of saddr event
)

var uidMap = map[string]string{}
var headerEndChar = []byte{")"[0]}
var headerSepChar = byte(':')
var spaceChar = byte(' ')

type AuditMessage struct {
	Type      uint16 `json:"type"`
	Data      string `json:"data"`
	Seq       int    `json:"-"`
	AuditTime string `json:"-"`
}

type AuditMessageGroup struct {
	Seq           int               `json:"sequence"`
	AuditTime     string            `json:"timestamp"`
	CompleteAfter time.Time         `json:"-"`
	Msgs          []*AuditMessage   `json:"messages"`
	UidMap        map[string]string `json:"uid_map"`
	DnsMap        map[string]string `json:"dnstap"`
	Syscall       string            `json:"-"`
	gotSaddr      bool
	gotDNS        bool
}

// Creates a new message group from the details parsed from the message
func NewAuditMessageGroup(am *AuditMessage) *AuditMessageGroup {
	//TODO: allocating 6 msgs per group is lame and we _should_ know ahead of time roughly how many we need
	amg := &AuditMessageGroup{
		Seq:           am.Seq,
		AuditTime:     am.AuditTime,
		CompleteAfter: time.Now().Add(COMPLETE_AFTER),
		UidMap:        make(map[string]string, 2), // Usually only 2 individual uids per execve
		DnsMap:        make(map[string]string, 1),
		Msgs:          make([]*AuditMessage, 0, 6),
	}

	amg.AddMessage(am)
	return amg
}

// Creates a new go-audit message from a netlink message
func NewAuditMessage(nlm *syscall.NetlinkMessage) *AuditMessage {
	aTime, seq := parseAuditHeader(nlm)
	return &AuditMessage{
		Type:      nlm.Header.Type,
		Data:      string(nlm.Data),
		Seq:       seq,
		AuditTime: aTime,
	}
}

// Gets the timestamp and audit sequence id from a netlink message
func parseAuditHeader(msg *syscall.NetlinkMessage) (time string, seq int) {
	headerStop := bytes.Index(msg.Data, headerEndChar)
	// If the position the header appears to stop is less than the minimum length of a header, bail out
	if headerStop < HEADER_MIN_LENGTH {
		return
	}

	header := string(msg.Data[:headerStop])
	if header[:HEADER_START_POS] == "audit(" {
		//TODO: out of range check, possibly fully binary?
		sep := strings.IndexByte(header, headerSepChar)
		time = header[HEADER_START_POS:sep]
		seq, _ = strconv.Atoi(header[sep+1:])

		// Remove the header from data
		msg.Data = msg.Data[headerStop+3:]
	}

	return time, seq
}

// Add a new message to the current message group
func (amg *AuditMessageGroup) AddMessage(am *AuditMessage) {
	amg.Msgs = append(amg.Msgs, am)
	//TODO: need to find more message types that won't contain uids, also make these constants
	switch am.Type {
	case EXECVE, CWD:
		// Don't map uids here
	case SOCKADDR:
		amg.mapDns(am)
	case SYSCALL:
		amg.findSyscall(am)
		amg.mapUids(am)
	default:
		amg.mapUids(am)
	}
}

// Find all `saddr=` occurrences in a message and do a lookup
func (amg *AuditMessageGroup) mapDns(am *AuditMessage) (ip string, host []byte) {
	data := am.Data
	start := 0
	end := 0

	if start = strings.Index(data, "saddr="); start < 0 {
		return
	}

	// Progress the start point beyond the = sign
	start += 6
	if end = strings.IndexByte(data[start:], spaceChar); end < 0 {
		end = len(data) - start
		if end > SOCKADDR_LENGTH {
			return
		}
	}

	saddr := data[start : start+end]

	amg.gotSaddr = true

	var err error

	ip = parseAddr(saddr)

	host, err = c.Get(ip)
	if err == nil {
		amg.gotDNS = true
		amg.DnsMap[ip] = string(host)
		//amg.DnsMap["time"] = fmt.Sprintf("%v", time.Now().Unix())
	}
	return
}

func parseAddr(saddr string) (addr string) {
	switch family := saddr[0:4]; family {
	// 0200: ipv4
	case "0200":
		b, err := hex.DecodeString(saddr[8:16])
		if err != nil {
			el.Printf("unable to decode hex to bytes: %s", err)
		}
		addr = net.IP(b).String()
	}

	return addr
}

// Find all `uid=` occurrences in a message and adds the username to the UidMap object
func (amg *AuditMessageGroup) mapUids(am *AuditMessage) {
	data := am.Data
	start := 0
	end := 0

	for {
		if start = strings.Index(data, "uid="); start < 0 {
			break
		}

		// Progress the start point beyond the = sign
		start += 4
		if end = strings.IndexByte(data[start:], spaceChar); end < 0 {
			// There was no ending space, maybe the uid is at the end of the line
			end = len(data) - start

			// If the end of the line is greater than 5 characters away (overflows a 16 bit uint) then it can't be a uid
			if end > 5 {
				break
			}
		}

		uid := data[start : start+end]

		// Don't bother re-adding if the existing group already has the mapping
		if _, ok := amg.UidMap[uid]; !ok {
			amg.UidMap[uid] = getUsername(data[start : start+end])
		}

		// Find the next uid= if we have space for one
		next := start + end + 1
		if next >= len(data) {
			break
		}

		data = data[next:]
	}

}

func (amg *AuditMessageGroup) findSyscall(am *AuditMessage) {
	data := am.Data
	start := 0
	end := 0

	if start = strings.Index(data, "syscall="); start < 0 {
		return
	}

	// Progress the start point beyond the = sign
	start += 8
	if end = strings.IndexByte(data[start:], spaceChar); end < 0 {
		// There was no ending space, maybe the syscall id is at the end of the line
		end = len(data) - start

		// If the end of the line is greater than 5 characters away (overflows a 16 bit uint) then it can't be a syscall id
		if end > 5 {
			return
		}
	}

	amg.Syscall = data[start : start+end]
}

// Gets a username for a user id
func getUsername(uid string) string {
	uname := "UNKNOWN_USER"

	// Make sure we have a uid element to work with.
	// Give a default value in case we don't find something.
	if lUser, ok := uidMap[uid]; ok {
		uname = lUser
	} else {
		lUser, err := user.LookupId(uid)
		if err == nil {
			uname = lUser.Username
		}
		uidMap[uid] = uname
	}

	return uname
}
