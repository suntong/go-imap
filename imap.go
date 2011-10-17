package main

import (
	"crypto/tls"
	"os"
	"log"
	"strings"
	"fmt"
	"io"
	"strconv"
	"sync"
)

func init() {
	log.SetFlags(log.Ltime | log.Lshortfile)
}

func check(err os.Error) {
	if err != nil {
		panic(err)
	}
}

type Status int

const (
	OK Status = iota
	NO
	BAD
)

func (s Status) String() string {
	return []string{
		"OK",
		"NO",
		"BAD",
	}[s]
}

const (
	WildcardAny          = "%"
	WildcardAnyRecursive = "*"
)

type TriBool int

const (
	TriUnknown = TriBool(iota)
	TriTrue
	TriFalse
)

func (t TriBool) String() string {
	switch t {
	case TriTrue:
		return "true"
	case TriFalse:
		return "false"
	}
	return "unknown"
}

type tag int

const Untagged = tag(-1)

type Response struct {
	status Status
	text   string
	extra  []interface{}
}

func (r *Response) String() string {
	return fmt.Sprintf("%s %s", r.status, r.text)
}

type ResponseChan chan *Response

type IMAP struct {
	// Client thread.
	nextTag int

	unsolicited chan interface{}

	// Background thread.
	r        *Parser
	w        io.Writer

	lock    sync.Mutex
	pending map[tag]chan *Response
}

func NewIMAP() *IMAP {
	return &IMAP{pending: make(map[tag]chan *Response)}
}

func (imap *IMAP) Connect(hostport string) (string, os.Error) {
	conn, err := tls.Dial("tcp", hostport, nil)
	if err != nil {
		return "", err
	}

	imap.r = newParser(conn)//&LoggingReader{conn})
	imap.w = conn

	tag, err := imap.readTag()
	if err != nil {
		return "", err
	}
	if tag != Untagged {
		return "", fmt.Errorf("expected untagged server hello. got %q", tag)
	}

	status, text, err := imap.readStatus("")
	if status != OK {
		return "", fmt.Errorf("server hello %v %q", status, text)
	}

	imap.StartLoops()

	return text, nil
}

func (imap *IMAP) readTag() (tag, os.Error) {
	str, err := imap.r.readToken()
	if err != nil {
		return Untagged, err
	}
	if len(str) == 0 {
		return Untagged, os.NewError("read empty tag")
	}

	switch str[0] {
	case '*':
		return Untagged, nil
	case 'a':
		tagnum, err := strconv.Atoi(str[1:])
		if err != nil {
			return Untagged, err
		}
		return tag(tagnum), nil
	}

	return Untagged, fmt.Errorf("unexpected response %q", str)
}

func min(a int, b int) int {
	if a < b {
		return a
	}
	return b
}

func (imap *IMAP) Send(ch chan *Response, format string, args ...interface{}) os.Error {
	tag := tag(imap.nextTag)
	imap.nextTag++

	toSend := []byte(fmt.Sprintf("a%d %s\r\n", int(tag), fmt.Sprintf(format, args...)))

	if ch != nil {
		imap.lock.Lock()
		imap.pending[tag] = ch
		imap.lock.Unlock()
	}

	_, err := imap.w.Write(toSend)
	return err
}

func (imap *IMAP) SendSync(format string, args ...interface{}) (*Response, os.Error) {
	ch := make(chan *Response, 1)
	err := imap.Send(ch, format, args...)
	if err != nil {
		return nil, err
	}
	response := <-ch
	return response, nil
}

func (imap *IMAP) Auth(user string, pass string) (*Response, os.Error) {
	return imap.SendSync("LOGIN %s %s", user, pass)
}

func quote(in string) string {
	if strings.IndexAny(in, "\r\n") >= 0 {
		panic("invalid characters in string to quote")
	}
	return "\"" + in + "\""
}

func (imap *IMAP) List(reference string, name string) (*Response, []*ResponseList, os.Error) {
	/* Responses:  untagged responses: LIST */
	response, err := imap.SendSync("LIST %s %s", quote(reference), quote(name))
	if err != nil {
		return nil, nil, err
	}

	extras := make([]interface{}, 0)
	lists := make([]*ResponseList, 0)
	for _, extra := range response.extra {
		if list, ok := extra.(*ResponseList); ok {
			lists = append(lists, list)
		} else {
			extras = append(extras, extra)
		}
	}
	response.extra = extras
	return response, lists, nil
}

type ResponseExamine struct {
	*Response
	flags []string
	exists int
	recent int
}

func (imap *IMAP) Examine(mailbox string) (*ResponseExamine, os.Error) {
	/*
	 Responses:  REQUIRED untagged responses: FLAGS, EXISTS, RECENT
	 REQUIRED OK untagged responses:  UNSEEN,  PERMANENTFLAGS,
	 UIDNEXT, UIDVALIDITY
	 */
	resp, err := imap.SendSync("EXAMINE %s", quote(mailbox))
	if err != nil {
		return nil, err
	}

	r := &ResponseExamine{Response: resp}

	extras := make([]interface{}, 0)
	for _, extra := range r.extra {
		switch extra := extra.(type) {
		case (*ResponseFlags):
			r.flags = extra.flags
		case (*ResponseExists):
			r.exists = extra.count
		case (*ResponseRecent):
			r.recent = extra.count
		default:
			extras = append(extras, extra)
		}
	}
	r.extra = extras
	return r, nil
}

func (imap *IMAP) Fetch(sequence string, fields []string) (*Response, []*ResponseFetch, os.Error) {
	var fieldsStr string
	if len(fields) == 1 {
		fieldsStr = fields[0]
	} else {
		fieldsStr = "\"" + strings.Join(fields, " ") + "\""
	}
	resp, err := imap.SendSync("FETCH %s %s", sequence, fieldsStr)
	if err != nil {
		return nil, nil, err
	}

	extras := make([]interface{}, 0)
	lists := make([]*ResponseFetch, 0)
	for _, extra := range resp.extra {
		if list, ok := extra.(*ResponseFetch); ok {
			lists = append(lists, list)
		} else {
			extras = append(extras, extra)
		}
	}
	resp.extra = extras
	return resp, lists, nil
}

func (imap *IMAP) StartLoops() {
	go func() {
		err := imap.ReadLoop()
		panic(err)
	}()
}

func (imap *IMAP) ReadLoop() os.Error {
	var untagged []interface{}
	for {
		tag, err := imap.readTag()
		if err != nil {
			return err
		}

		if tag == Untagged {
			resp, err := imap.readUntagged()
			if err != nil {
				return err
			}

			if untagged == nil {
				imap.lock.Lock()
				hasPending := len(imap.pending) > 0
				imap.lock.Unlock()

				if hasPending {
					untagged = make([]interface{}, 0, 1)
				}
			}

			if untagged != nil {
				untagged = append(untagged, resp)
			} else {
				imap.unsolicited <- resp
			}
		} else {
			status, text, err := imap.readStatus("")
			if err != nil {
				return err
			}

			imap.lock.Lock()
			ch := imap.pending[tag]
			imap.pending[tag] = nil, false
			imap.lock.Unlock()

			ch <- &Response{status:status, text:text, extra:untagged}
			untagged = nil
		}
	}

	panic("not reached")
}

func (imap *IMAP) readStatus(code string) (Status, string, os.Error) {
	if len(code) == 0 {
		var err os.Error
		code, err = imap.r.readToken()
		if err != nil {
			return BAD, "", err
		}
	}

	// TODO: response code
	codes := map[string]Status{
		"OK":  OK,
		"NO":  NO,
		"BAD": BAD,
	}

	status, known := codes[code]
	if !known {
		return BAD, "", fmt.Errorf("unexpected status %q", code)
	}

	rest, err := imap.r.readToEOL()
	if err != nil {
		return BAD, "", err
	}

	return status, rest, nil
}

type ResponseCapabilities struct {
	caps []string
}

type ResponseList struct {
	inferiors  TriBool
	selectable TriBool
	marked     TriBool
	children   TriBool
	delim      string
	mailbox    string
}

type ResponseFlags struct {
	flags []string
}

type ResponseExists struct {
	count int
}
type ResponseRecent struct {
	count int
}

type Address struct {
	name, source, address string
}

func (a *Address) FromSexp(s []Sexp) {
	if name := nilOrString(s[0]); name != nil {
		a.name = *name
	}
	if source := nilOrString(s[1]); source != nil {
		a.source = *source
	}
	mbox := nilOrString(s[2])
	host := nilOrString(s[3])
	if mbox != nil && host != nil {
		address := *mbox + "@" + *host
		a.address = address
	}
}
func AddressListFromSexp(s Sexp) []Address {
	if s == nil {
		return nil
	}

	saddrs := s.([]Sexp)
	addrs := make([]Address, len(saddrs))
	for i, s := range saddrs {
		addrs[i].FromSexp(s.([]Sexp))
	}
	return addrs
}

type ResponseFetchEnvelope struct {
	date, subject, inReplyTo, messageId *string
	from, sender, replyTo, to, cc, bcc  []Address
}

type ResponseFetch struct {
	msg          int
	flags        Sexp
	envelope     ResponseFetchEnvelope
	internalDate string
	size         int
}

func (imap *IMAP) readUntagged() (resp interface{}, outErr os.Error) {
	defer func() {
		if e := recover(); e != nil {
			if osErr, ok := e.(os.Error); ok {
				outErr = osErr
				return
			}
			panic(e)
		}
	}()

	command, err := imap.r.readToken()
	check(err)

	switch command {
	case "CAPABILITY":
		caps := make([]string, 0)
		for {
			cap, err := imap.r.readToken()
			check(err)
			if len(cap) == 0 {
				break
			}
			caps = append(caps, cap)
		}
		check(imap.r.expectEOL())
		return &ResponseCapabilities{caps}, nil

	case "LIST":
		// "(" [mbx-list-flags] ")" SP (DQUOTE QUOTED-CHAR DQUOTE / nil) SP mailbox
		flags, err := imap.r.readParenStringList()
		check(err)
		imap.r.expect(" ")

		delim, err := imap.r.readQuoted()
		check(err)
		imap.r.expect(" ")

		mailbox, err := imap.r.readQuoted()
		check(err)

		check(imap.r.expectEOL())

		list := &ResponseList{delim: string(delim), mailbox: string(mailbox)}
		for _, flag := range flags {
			switch flag {
			case "\\Noinferiors":
				list.inferiors = TriFalse
			case "\\Noselect":
				list.selectable = TriFalse
			case "\\Marked":
				list.marked = TriTrue
			case "\\Unmarked":
				list.marked = TriFalse
			case "\\HasChildren":
				list.children = TriTrue
			case "\\HasNoChildren":
				list.children = TriFalse
			default:
				return nil, fmt.Errorf("unknown list flag %q", flag)
			}
		}
		return list, nil

	case "FLAGS":
		flags, err := imap.r.readParenStringList()
		check(err)

		check(imap.r.expectEOL())

		return &ResponseFlags{flags}, nil

	case "OK", "NO", "BAD":
		status, text, err := imap.readStatus(command)
		check(err)
		return &Response{status, text, nil}, nil
	}

	num, err := strconv.Atoi(command)
	if err == nil {
		command, err := imap.r.readToken()
		check(err)

		switch command {
		case "EXISTS":
			check(imap.r.expectEOL())
			return &ResponseExists{num}, nil
		case "RECENT":
			check(imap.r.expectEOL())
			return &ResponseRecent{num}, nil
		case "FETCH":
			sexp, err := imap.r.readSexp()
			check(err)
			if len(sexp)%2 != 0 {
				panic("fetch sexp must have even number of items")
			}
			fetch := &ResponseFetch{msg: num}
			for i := 0; i < len(sexp); i += 2 {
				key := sexp[i].(string)
				switch key {
				case "ENVELOPE":
					env := sexp[i+1].([]Sexp)
					// This format is insane.
					if len(env) != 10 {
						return nil, fmt.Errorf("envelope needed 10 fields, had %d", len(env))
					}
					fetch.envelope.date = nilOrString(env[0])
					fetch.envelope.subject = nilOrString(env[1])
					fetch.envelope.from = AddressListFromSexp(env[2])
					fetch.envelope.sender = AddressListFromSexp(env[3])
					fetch.envelope.replyTo = AddressListFromSexp(env[4])
					fetch.envelope.to = AddressListFromSexp(env[5])
					fetch.envelope.cc = AddressListFromSexp(env[6])
					fetch.envelope.bcc = AddressListFromSexp(env[7])
					fetch.envelope.inReplyTo = nilOrString(env[8])
					fetch.envelope.messageId = nilOrString(env[9])
				case "FLAGS":
					fetch.flags = sexp[i+1]
				case "INTERNALDATE":
					fetch.internalDate = sexp[i+1].(string)
				case "RFC822.SIZE":
					fetch.size, err = strconv.Atoi(sexp[i+1].(string))
					if err != nil {
						return nil, err
					}
				default:
					panic(fmt.Sprintf("unhandled key %#v", key))
				}
			}
			check(imap.r.expectEOL())
			return fetch, nil
		}
	}

	return nil, fmt.Errorf("unhandled untagged response %s", command)
}
