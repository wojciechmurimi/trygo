// HTTP-message   = start-line CRLF *( field-line CRLF ) CRLF [ message-body ]
// start-line     = request-line | status-line

// request-line   = method SP request-target SP HTTP-version

// method         = token
// request-target = origin-form | absolute-form | authority-form | asterisk-form
// origin-form    = absolute-path [ "?" query ]
// absolute-form  = absolute-URI
// authority-form = uri-host ":" port
// asterisk-form  = "*"

// status-line = HTTP-version SP status-code SP [ reason-phrase ]

// HTTP-version  = HTTP-name "/" DIGIT "." DIGIT
// HTTP-name     = %s"HTTP"
// status-code    = 3DIGIT
// reason-phrase  = 1*( HTAB / SP / VCHAR / obs-text )

// field-line   = field-name ":" OWS field-value OWS

// // obs-fold     = OWS CRLF RWS
// //				; obsolete line folding

// message-body = *OCTET
// Transfer-Encoding = #transfer-coding

// chunked-body   = *chunk last-chunk trailer-section CRLF
// chunk          = chunk-size [ chunk-ext ] CRLF chunk-data CRLF
// chunk-size     = 1*HEXDIG
// last-chunk     = 1*("0") [ chunk-ext ] CRLF
// chunk-data     = 1*OCTET

// chunk-ext      = *( BWS ";" BWS chunk-ext-name [ BWS "=" BWS chunk-ext-val ] )
// chunk-ext-name = token
// chunk-ext-val  = token / quoted-string
// trailer-section   = *( field-line CRLF )

package trygo

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)



type Conn interface {
	Read(b []byte) (int, error);
	Write(b []byte) (int, error);
	Close() error
}

type RealBuffer struct {
	stream Conn
}

func (self *RealBuffer) Write(buff []byte) (int, error) {
	return self.stream.Write(buff)
}

func (self *RealBuffer) Read(buff []byte) (int, error) {
	n, e := self.stream.Read(buff[:])
	if e != nil {
		return n, e
	}
	return n, nil
}

func (self *RealBuffer) readLine(buff []byte) (int, error) {
	var index = 0
	for {
		var b [1]byte
		n, e := self.stream.Read(b[:])
		if e != nil {
			return n, e
		}
		if b[0] == '\n' {
			break
		}
		buff[index] = b[0]
		index += n

		if index >= int(len(buff)) {
			break
		}
	}

	return index, nil
}

type HTTPMethod = uint64

var (
	GET     = parse("GET")
	HEAD    = parse("HEAD")
	POST    = parse("POST")
	PUT     = parse("PUT")
	DELETE  = parse("DELETE")
	CONNECT = parse("CONNECT")
	OPTIONS = parse("OPTIONS")
	TRACE   = parse("TRACE")
	PATCH   = parse("PATCH")
)

func parse(str string) HTTPMethod {
	var arr [8]byte
	copy(arr[:], []byte(str))
	return binary.LittleEndian.Uint64(arr[:])
}

func requestHasBody(self HTTPMethod) bool {
	switch self {
	case POST, PUT, PATCH:
		return true
	case GET, HEAD, DELETE, CONNECT, OPTIONS, TRACE:
		return false
	default:
		return true
	}
}

func responseHasBody(self HTTPMethod) bool {
	switch self {
	case GET, POST, DELETE, CONNECT, OPTIONS, PATCH:
		return true
	case HEAD, PUT, TRACE:
		return false
	default:
		return true
	}
}

type MessageType int

const (
	REQUEST  MessageType = 0
	RESPONSE MessageType = 1
)

type Request struct {
	//message_type MessageType
	//http_version string
	uri         string
	http_method HTTPMethod
	headers     map[string][]string
	stream      HttpStream
	length      int
	status_code string
}

func (req *Request) Read(buff []byte) (int, error) {
	return req.stream.Read(buff);
}

func (req *Request) Write(buff []byte) (int, error) {
	return req.stream.Write(buff);
}

func (req *Request) Close() error {
	return req.stream.Close();
}

func (req *Request) Finished () bool {
	return req.stream.finished
}

type Unchunker struct {
	expecting int
	m         RealBuffer
}

type HttpStream struct {
	reader       io.ReadWriter
	finished     bool
	to_read      int
	already_read int
}

func (self *HttpStream) Close() error {
	switch v := self.reader.(type) {
	case *RealBuffer:
		return v.stream.Close()
	case *Unchunker:
		return v.m.stream.Close()
	default:
		return errors.New("invalid stream")
	}
}

func (self *HttpStream) Read(buff []byte) (int, error) {
	switch v := self.reader.(type) {
	case *RealBuffer:
		r, err := v.Read(buff)
		self.already_read += r
		if self.already_read >= self.to_read {
			self.finished = true
		}
		return r, err
	case *Unchunker:
		r, err := v.Read(buff)
		if err == io.EOF {
			self.finished = true
		}
		return r, err
	default:
		return 0, errors.New("invalid stream")
	}
}

func (self *HttpStream) Write(buff []byte) (int, error) {
	switch v := self.reader.(type) {
	case *RealBuffer:
		return v.Write(buff)
	case *Unchunker:
		return v.Write(buff)
	default:
		return 0, errors.New("invalid stream")
	}
}

type Encoding uint

const (
	CHUNKED     Encoding = 0
	CONTENT_LEN Encoding = 0
)

func min(a int, b int) int {
	if a > b {
		return b
	}
	return a
}

func (self *Unchunker) ReadChunk(my_buff []byte) (int, bool, error) {
	var buff [64]byte
	r, e := self.m.Read(my_buff[:min(self.expecting, len(my_buff))])

	if e != nil {
		return r, false, e
	}
	self.expecting -= r
	if self.expecting <= 0 {
		_, e = self.m.Read(buff[:2])
		if e != nil {
			return r, false, e
		}
		if !bytes.HasPrefix(buff[:], []byte{'\r', '\n'}) {
			fmt.Println(string(buff[:]))
			return 0, true, ChunkErrorFrom("expect chunk to end with CLRF")
		}
		return r, true, nil
	}

	return r, false, nil
}

func (self *Unchunker) Write(buff []byte) (int, error) {
	return self.m.Write(buff)
}

func (self *Unchunker) Read(my_buff []byte) (int, error) {
	var buff [64]byte
	var index = 0
	if self.expecting > 0 {
		r, _, err := self.ReadChunk(my_buff)
		if err != nil {
			return r, err
		}

		return r, nil
	}
	read, err := self.m.readLine(buff[:])
	if err != nil {
		return read, err
	}

	if read == 0 {
		return 0, ChunkErrorFrom("unexpected chunk length")
	}

	if buff[read-1] != '\r' {
		return 0, ChunkErrorFrom("expect line to end with CL")
	}
	expectedlen, err := strconv.ParseInt(string(buff[:read-1]), 16, 64)
	if err != nil {
		return 0, ChunkErrorFrom("failed to parse chunk length")
	}
	if expectedlen == 0 {
		return 0, io.EOF
	}
	self.expecting = int(expectedlen)
	len, complete, err := self.ReadChunk(my_buff[index:])
	if err != nil {
		return len, err
	}
	if complete {
		self.expecting = 0
	}
	return len, nil
}

func initRequest() Request {
	var message Request
	message.headers = make(map[string][]string)
	return message
}

func startsWith(byte_array []byte, word string) bool {
	if len(word) > len(byte_array) {
		return false
	}
	return bytes.Equal(byte_array[:len(word)], []byte(word))
}

type InvalidHeader struct {
	message string
}

type InvalidChunk struct {
	message string
}

func ChunkErrorFrom(str string) error {
	return &InvalidHeader{str}
}

func HeaderErrorFrom(str string) error {
	return &InvalidHeader{str}
}

func (self InvalidHeader) Error() string {
	return self.message
}

func readHead(conection Conn) (*Request, error) {
	//var me = "GET /hello.txt HTTP/1.1\r\ncontent-length: 3\r\n\r\n300"
	//var message = "a\nb\nc\n"
	//var me = "HTTP/1.1 200 OK\r\n"
	var mock = RealBuffer{conection}
	//initMockBuffer(me)
	var buff [4096]uint8
	var message = initRequest()
	message.headers = make(map[string][]string)
	regex := regexp.MustCompile(`HTTP\/[0-9]\.[0-9]`)
	regex_comma := regexp.MustCompile("[ ]*,[ ]*")

	for i := 0; ; i++ {
		read_bytes, err := mock.readLine(buff[:])
		if err != nil {
			return nil, nil
		}
		if read_bytes == 0 {
			break
		}
		var read_line = buff[:read_bytes]
		mf := "malformed headers"
		if read_line[len(read_line)-1] != '\r' {
			return nil, HeaderErrorFrom(mf)
		}
		read_line = buff[:read_bytes-1]
		if len(read_line) == 0 {
			break
		}
		if i == 0 {
			if startsWith(buff[:], "HTTP") {
				//message.message_type = RESPONSE
				var found_buff, rem, found = bytes.Cut(read_line[:read_bytes], []byte(" "))
				if !found {
					return nil, HeaderErrorFrom(mf)
				}
				if !regex.Match(found_buff) {
					return nil, HeaderErrorFrom(mf)
				}
				found_buff, rem, found = bytes.Cut(rem[:], []byte(" "))
				if !found {
					return nil, HeaderErrorFrom(mf)
				}
				message.status_code = string(found_buff);
				found_buff, _, _ = bytes.Cut(rem[:], []byte("\r"))
				if len(found_buff) == 0 {
					return nil, HeaderErrorFrom(mf)
				}
			} else {
				//message.message_type = REQUEST
				var found_buff, rem, found = bytes.Cut(read_line[:read_bytes], []byte(" "))
				if !found {
					return nil, HeaderErrorFrom(mf)
				}
				message.http_method = parse(string(found_buff))
				found_buff, rem, found = bytes.Cut(rem[:], []byte(" "))
				if !found {
					return nil, HeaderErrorFrom(mf)
				}
				message.uri = string(found_buff);
				found_buff, _, _ = bytes.Cut(rem[:], []byte("\r"))
				if len(found_buff) == 0 {
					return nil, HeaderErrorFrom(mf)
				}
				if !regex.Match(found_buff) {
					return nil, HeaderErrorFrom(mf)
				}
			}
		} else {
			var field_name, rem, found = bytes.Cut(read_line, []byte(":"))

			if !found {
				return nil, HeaderErrorFrom(mf)
			}
			if field_name[len(field_name)-1] == ' ' {
				return nil, HeaderErrorFrom(mf)
			}
			rem = bytes.TrimSpace(rem)
			field_values := regex_comma.Split(string(rem), -1)
			message.headers[strings.ToLower(string(field_name))] = field_values
		}
	}

	if requestHasBody(message.http_method) {
		content_len_value := message.headers["content-length"]
		transfer_encoding := message.headers["transfer-encoding"]

		if content_len_value != nil {
			content_len, err := strconv.ParseUint(content_len_value[0], 10, 32)
			if err != nil {
				return nil, HeaderErrorFrom("failed to parse content length")
			}
			message.length = int(content_len)
			message.stream = HttpStream{&RealBuffer{conection}, false, int(content_len), 0}
		} else {
			if transfer_encoding == nil {
				return nil, HeaderErrorFrom("content encoding not provided")
			}

			if transfer_encoding[len(transfer_encoding)-1] == "chunked" {
				message.stream = HttpStream{&Unchunker{0, RealBuffer{conection}}, false, 0, 0}
			} else {
				return nil, HeaderErrorFrom("content length not provided")
			}
		}
	}

	return &message, nil
}

type HttpServer struct {
	listener net.Listener
}

func CreateHTTPServer(listener net.Listener) HttpServer {
	return HttpServer{listener}
}

func (self *HttpServer) Accept() (*Request, error) {
	con, err := self.listener.Accept()
	if err != nil {
		return nil, nil
	}
	message, err := readHead(con)
	return message, nil
}

type ResponseBuilder struct {
	status_code string
	headers     map[string]string
}

func (self *ResponseBuilder) SetCode(code uint16) *ResponseBuilder {
	self.status_code = fmt.Sprintf("%d", code)
	return self
}

func (self *ResponseBuilder) SetHeader(field string, value string) *ResponseBuilder {
	if self.headers == nil {
		self.headers = make(map[string]string)
	}
	val, ok := self.headers[field]
	if ok {
		self.headers[field] = fmt.Sprintf("%s, %s", val, value)
	} else {
		self.headers[field] = value
	}
	return self
}

func (self *ResponseBuilder) String() string {
	if self.status_code == "" {
		self.status_code = "200"
	}
	var s = fmt.Sprintf("HTTP/1.1 %s  \r\n", self.status_code)
	var builder strings.Builder
	builder.WriteString(s)
	for key, val := range self.headers {
		builder.WriteString(fmt.Sprintf("%s: %s\r\n", key, val))
	}
	builder.WriteString("\r\n")
	return builder.String()
}

type HTTPClient struct {
	conn    Conn
	headers map[string]string
}

func iSRedirectionCode(code string) bool {
	return code == "301" || code == "302" || code == "303" || code == "307" || code == "308"
}

func (self *HTTPClient) Connect(u string) (*Request, error) {
	if self.headers == nil {
		self.headers = make(map[string]string)
	}
	uri, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	if uri.Host == "" {
		return nil, errors.New("no host")
	}
	self.headers["Host"] = uri.Host
	self.headers["User-Agent"] = "trygo v 1.1"
	self.headers["Connection"] = "Close"

	url := uri.RequestURI();
	if uri.Fragment != "" {
		url = fmt.Sprintf("%s#%s", url, uri.Fragment)
	}
	c, err := net.Dial("tcp", fmt.Sprintf("%s:443", uri.Host))
	if err != nil {
		return nil, err
	}
	if uri.Scheme == "https" {
		self.conn = tls.Client(c, &tls.Config{InsecureSkipVerify: true})
	} else {
		self.conn = c
	}
	var request strings.Builder
	request.WriteString(fmt.Sprintf("GET %s HTTP/1.1\r\n", url))
	for key, value := range self.headers {
		request.WriteString(fmt.Sprintf("%s: %s\r\n", key, value))
	}
	request.WriteString("\r\n");
	_, err = self.conn.Write([]byte(request.String()));
	if err != nil {
		return nil, err
	}
	res, err := readHead(self.conn);
	if err != nil {
		return nil, err
	}
	if iSRedirectionCode(res.status_code) {
		rd, ok := res.headers["location"]
		if ok {
			res, err = self.Connect(rd[0])
		}
	}
	return res, err
}
