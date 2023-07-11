package sse

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/tmaxmax/go-sse/internal/parser"
)

func isSingleLine(p string) bool {
	_, newlineLen := parser.NewlineIndex(p)
	return newlineLen == 0
}

// fieldBytes holds the byte representation of each field type along with a colon at the end.
var (
	fieldBytesData    = []byte(parser.FieldNameData + ": ")
	fieldBytesEvent   = []byte(parser.FieldNameEvent + ": ")
	fieldBytesRetry   = []byte(parser.FieldNameRetry + ": ")
	fieldBytesID      = []byte(parser.FieldNameID + ": ")
	fieldBytesComment = []byte(": ")
)

type chunk struct {
	content   string
	isComment bool
}

var newline = []byte{'\n'}

func (c *chunk) WriteTo(w io.Writer) (int64, error) {
	name := fieldBytesData
	if c.isComment {
		name = fieldBytesComment
	}
	n, err := w.Write(name)
	if err != nil {
		return int64(n), err
	}
	m, err := writeString(w, c.content)
	n += m
	if err != nil {
		return int64(n), err
	}
	m, err = w.Write(newline)
	return int64(n + m), err
}

// Message is the representation of a single message sent from the server to its clients.
//
// A Message is made of a single event, which is sent to each client, and other metadata
// about the message itself: its expiry time and topic.
//
// Topics are used to filter which events reach which clients. If a client subscribes to
// a certain set of topics, but a message's topic is not part of that set, then the underlying
// event of the message does not reach the client.
//
// The message's expiry time can be used when replaying messages to new clients. If a message
// is expired, then it is not sent. Replay providers will usually make use of this.
//
// The Topic and ExpiresAt fields are used only on the server and are not sent to the client.
// They are not part of the protocol.
type Message struct {
	Topic     string
	ExpiresAt time.Time

	ID   EventID
	Name EventName

	chunks     []chunk
	retryValue string
}

func (e *Message) appendText(isComment bool, chunks ...string) {
	for _, c := range chunks {
		var content string

		for c != "" {
			content, c, _ = parser.NextChunk(c)
			e.chunks = append(e.chunks, chunk{content: content, isComment: isComment})
		}
	}
}

// AppendData creates multiple data fields on the message's event from the given strings.
//
// Server-sent events are not suited for binary data: the event fields are delimited by newlines,
// where a newline can be a LF, CR or CRLF sequence. When the client interprets the fields,
// it joins multiple data fields using LF, so information is altered. Here's an example:
//
//	initial payload: This is a\r\nmultiline\rtext.\nIt has multiple\nnewline\r\nvariations.
//	data sent over the wire:
//		data: This is a
//		data: multiline
//		data: text.
//		data: It has multiple
//		data: newline
//		data: variations
//	data received by client: This is a\nmultiline\ntext.\nIt has multiple\nnewline\nvariations.
//
// Each line prepended with "data:" is a field; multiple data fields are joined together using LF as the delimiter.
// If you attempted to send the same payload without prepending the "data:" prefix, like so:
//
//	data: This is a
//	multiline
//	text.
//	It has multiple
//	newline
//	variations
//
// there would be only one data field (the first one). The rest would be different fields, named "multiline", "text.",
// "It has multiple" etc., which are invalid fields according to the protocol.
//
// Besides, the protocol explicitly states that event streams must always be UTF-8 encoded:
// https://html.spec.whatwg.org/multipage/server-sent-events.html#parsing-an-event-stream.
//
// If you need to send binary data, you can use a Base64 encoder or any other encoder that does not output
// any newline characters (\r or \n) and then append the resulted data.
//
// Given that clients treat all newlines the same and replace the original newlines with LF,
// for internal code simplicity AppendData replaces them aswell.
func (e *Message) AppendData(chunks ...string) {
	e.appendText(false, chunks...)
}

// Comment creates a comment field on the message's event. If it spans multiple lines,
// new comment lines are created.
func (e *Message) Comment(comments ...string) {
	e.appendText(true, comments...)
}

// SetRetry creates a field on the message's event that tells the client to set the event stream reconnection time to
// the number of milliseconds it provides.
func (e *Message) SetRetry(duration time.Duration) {
	e.retryValue = strconv.FormatInt(duration.Milliseconds(), 10)
}

func (e *Message) writeMessageField(w io.Writer, f messageField, fieldBytes []byte) (int64, error) {
	if !f.IsSet() {
		return 0, nil
	}

	n, err := w.Write(fieldBytes)
	if err != nil {
		return int64(n), err
	}
	m, err := writeString(w, f.String())
	n += m
	if err != nil {
		return int64(n), err
	}
	m, err = w.Write(newline)
	return int64(n + m), err
}

func (e *Message) writeID(w io.Writer) (int64, error) {
	return e.writeMessageField(w, e.ID.messageField, fieldBytesID)
}

func (e *Message) writeName(w io.Writer) (int64, error) {
	return e.writeMessageField(w, e.Name.messageField, fieldBytesEvent)
}

func (e *Message) writeRetry(w io.Writer) (int64, error) {
	if e.retryValue == "" {
		return 0, nil
	}

	n, err := w.Write(fieldBytesRetry)
	if err != nil {
		return int64(n), err
	}
	m, err := writeString(w, e.retryValue)
	n += m
	if err != nil {
		return int64(n), err
	}
	m, err = w.Write(newline)
	return int64(n + m), err
}

// WriteTo writes the standard textual representation of the message's event to an io.Writer.
// This operation is heavily optimized and does zero allocations, so it is strongly preferred
// over MarshalText or String.
func (e *Message) WriteTo(w io.Writer) (int64, error) {
	n, err := e.writeID(w)
	if err != nil {
		return n, err
	}
	m, err := e.writeName(w)
	n += m
	if err != nil {
		return n, err
	}
	m, err = e.writeRetry(w)
	n += m
	if err != nil {
		return n, err
	}
	for i := range e.chunks {
		m, err = e.chunks[i].WriteTo(w)
		n += m
		if err != nil {
			return n, err
		}
	}
	o, err := w.Write(newline)
	return int64(o) + n, err
}

// MarshalText writes the standard textual representation of the message's event. Marshalling and unmarshalling will
// result in a message with an event that has the same fields; expiry time and topic will be lost.
//
// If you want to preserve everything, create your own custom marshalling logic.
// For an example using encoding/json, see the top-level MessageCustomJSONMarshal example.
//
// Use the WriteTo method if you don't need the byte representation.
//
// The representation is written to a bytes.Buffer, which means the error is always nil.
// If the buffer grows to a size bigger than the maximum allowed, MarshalText will panic.
// See the bytes.Buffer documentation for more info.
func (e *Message) MarshalText() ([]byte, error) {
	b := bytes.Buffer{}
	_, err := e.WriteTo(&b)
	return b.Bytes(), err
}

// String writes the message's event standard textual representation to a strings.Builder and returns the resulted string.
// It may panic if the representation is too long to be buffered.
//
// Use the WriteTo method if you don't actually need the string representation.
func (e *Message) String() string {
	s := strings.Builder{}
	_, _ = e.WriteTo(&s)
	return s.String()
}

// UnmarshalError is the error returned by the Message's UnmarshalText method.
// If the error is related to a specific field, FieldName will be a non-empty string.
// If no fields were found in the target text or any other errors occurred, only
// a Reason will be provided. Reason is always present.
type UnmarshalError struct {
	Reason    error
	FieldName string
	// The value of the invalid field.
	FieldValue string
}

func (u *UnmarshalError) Error() string {
	if u.FieldName == "" {
		return fmt.Sprintf("unmarshal event error: %s", u.Reason.Error())
	}
	return fmt.Sprintf("unmarshal event error, %s field invalid: %s. contents: %s", u.FieldName, u.Reason.Error(), u.FieldValue)
}

func (u *UnmarshalError) Unwrap() error {
	return u.Reason
}

// ErrUnexpectedEOF is returned when unmarshaling a Message from an input that doesn't end in a newline.
var ErrUnexpectedEOF = parser.ErrUnexpectedEOF

func (e *Message) reset() {
	e.chunks = nil
	e.Name = EventName{}
	e.ID = EventID{}
	e.retryValue = ""
}

// UnmarshalText extracts the first event found in the given byte slice into the
// receiver. The input is expected to be a wire format event, as defined by the spec.
// Therefore, previous fields present on the Message will be overwritten
// (i.e. event, ID, comments, data, retry), but the Topic and ExpiresAt will be kept as is,
// as these are not event fields.
//
// A method for marshalling and unmarshalling Messages together with their Topic and ExpiresAt
// can be seen in the top-level example MessageCustomJSONMarshal.
//
// Unmarshaling ignores fields with invalid names. If no valid fields are found,
// an error is returned. For a field to be valid it must end in a newline - if the last
// field of the event doesn't end in one, an error is returned.
//
// All returned errors are of type UnmarshalError.
func (e *Message) UnmarshalText(p []byte) error {
	e.reset()

	s := parser.NewFieldParser(string(p))
	s.KeepComments(true)

loop:
	for f := (parser.Field{}); s.Next(&f); {
		switch f.Name {
		case parser.FieldNameRetry:
			if i := strings.IndexFunc(f.Value, func(r rune) bool {
				return r < '0' || r > '9'
			}); i != -1 {
				r, _ := utf8.DecodeRuneInString(f.Value[i:])

				return &UnmarshalError{
					FieldName:  string(f.Name),
					FieldValue: f.Value,
					Reason:     fmt.Errorf("contains character %q, which is not an ASCII digit", r),
				}
			}

			e.retryValue = f.Value
		case parser.FieldNameData, parser.FieldNameComment:
			e.chunks = append(e.chunks, chunk{content: f.Value, isComment: f.Name == parser.FieldNameComment})
		case parser.FieldNameEvent:
			e.Name.value = f.Value
			e.Name.set = true
		case parser.FieldNameID:
			if strings.IndexByte(f.Value, 0) != -1 {
				break
			}

			e.ID.value = f.Value
			e.ID.set = true
		default: // event end
			break loop
		}
	}

	if len(e.chunks) == 0 && !e.Name.IsSet() && e.retryValue == "" && !e.ID.IsSet() || s.Err() != nil {
		e.reset()
		return &UnmarshalError{Reason: ErrUnexpectedEOF}
	}
	return nil
}

// Clone returns a copy of the message.
func (e *Message) Clone() *Message {
	return &Message{
		Topic:     e.Topic,
		ExpiresAt: e.ExpiresAt,
		// The first AppendData will trigger a reallocation.
		// Already appended chunks cannot be modified/removed, so this is safe.
		chunks:     e.chunks[:len(e.chunks):len(e.chunks)],
		retryValue: e.retryValue,
		Name:       e.Name,
		ID:         e.ID,
	}
}

func writeString(w io.Writer, s string) (int, error) {
	return w.Write(unsafe.Slice((*byte)(unsafe.Pointer((*reflect.StringHeader)(unsafe.Pointer(&s)).Data)), len(s)))
}
