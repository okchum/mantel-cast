package cast

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestCastMessageRoundTrip(t *testing.T) {
	cases := []*castMessage{
		{SourceID: "sender-0", DestinationID: "receiver-0", Namespace: nsReceiver, PayloadUTF8: `{"type":"GET_STATUS","requestId":1}`},
		{SourceID: "a", DestinationID: "b", Namespace: "urn:x-cast:test", PayloadType: payloadBinary, PayloadBinary: []byte{0, 1, 2, 0xff}},
		{}, // all zero values: required fields still encoded
		{SourceID: string(bytes.Repeat([]byte("x"), 300)), PayloadUTF8: "ünïcode ✓"},
	}
	for i, m := range cases {
		got, err := unmarshalCastMessage(m.marshal())
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, m) {
			t.Fatalf("case %d: round trip mismatch\n got %+v\nwant %+v", i, got, m)
		}
	}
}

func TestCastMessageKnownEncoding(t *testing.T) {
	m := &castMessage{SourceID: "s", DestinationID: "d", Namespace: "n", PayloadUTF8: "p"}
	want := []byte{
		0x08, 0x00, // 1: protocol_version = 0
		0x12, 0x01, 's', // 2: source_id
		0x1a, 0x01, 'd', // 3: destination_id
		0x22, 0x01, 'n', // 4: namespace
		0x28, 0x00, // 5: payload_type = STRING
		0x32, 0x01, 'p', // 6: payload_utf8
	}
	if got := m.marshal(); !bytes.Equal(got, want) {
		t.Fatalf("marshal = % x, want % x", got, want)
	}
}

func TestCastMessageSkipsUnknownFields(t *testing.T) {
	b := (&castMessage{SourceID: "s", Namespace: "n", PayloadUTF8: "p"}).marshal()
	b = appendTag(b, 20, wireVarint)
	b = appendVarint(b, 1<<40)
	b = appendTag(b, 21, wireFixed32)
	b = append(b, 1, 2, 3, 4)
	b = appendTag(b, 22, wireFixed64)
	b = append(b, 1, 2, 3, 4, 5, 6, 7, 8)
	b = appendStringField(b, 23, "ignored")
	m, err := unmarshalCastMessage(b)
	if err != nil {
		t.Fatal(err)
	}
	if m.SourceID != "s" || m.Namespace != "n" || m.PayloadUTF8 != "p" {
		t.Fatalf("unexpected decode %+v", m)
	}
}

func TestCastMessageMalformed(t *testing.T) {
	bad := [][]byte{
		{0x12, 0x05, 'a'},                    // length beyond buffer
		{0x08},                               // missing varint
		{0x0b},                               // wire type 3 (groups) unsupported
		bytes.Repeat([]byte{0xff}, 11),       // varint overflow
		{0x00, 0x00},                         // field 0
		{0x0d, 0x01, 0x02},                   // fixed32 truncated
		{0x09, 0x01, 0x02, 0x03, 0x04},       // fixed64 truncated
		{0x12, 0xff, 0xff, 0xff, 0xff, 0x0f}, // huge length
	}
	for i, b := range bad {
		if _, err := unmarshalCastMessage(b); err == nil {
			t.Errorf("case %d (% x): expected error", i, b)
		}
	}
}

func TestFraming(t *testing.T) {
	var buf bytes.Buffer
	msgs := []*castMessage{
		{SourceID: "sender-0", DestinationID: "receiver-0", Namespace: nsHeartbeat, PayloadUTF8: `{"type":"PING"}`},
		{SourceID: "receiver-0", DestinationID: "sender-0", Namespace: nsHeartbeat, PayloadUTF8: `{"type":"PONG"}`},
	}
	for _, m := range msgs {
		if err := writeFrame(&buf, m); err != nil {
			t.Fatal(err)
		}
	}
	raw := buf.Bytes()
	if n := binary.BigEndian.Uint32(raw); int(n) != len(msgs[0].marshal()) {
		t.Fatalf("length prefix %d, want %d", n, len(msgs[0].marshal()))
	}
	r := bytes.NewReader(raw)
	for i, want := range msgs {
		got, err := readFrame(r)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("frame %d mismatch: %+v", i, got)
		}
	}
	if _, err := readFrame(r); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}

	// Truncated body.
	if _, err := readFrame(bytes.NewReader(raw[:10])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected ErrUnexpectedEOF, got %v", err)
	}
	// Oversized length prefix.
	if _, err := readFrame(bytes.NewReader([]byte{0x7f, 0xff, 0xff, 0xff})); err == nil {
		t.Fatal("expected error for oversized frame")
	}
}

func TestVarintTenthByte(t *testing.T) {
	maxV := append(bytes.Repeat([]byte{0xff}, 9), 0x01)
	if v, n, err := readVarint(maxV); err != nil || n != 10 || v != ^uint64(0) {
		t.Fatalf("max uint64: %d %d %v", v, n, err)
	}
	over := append(bytes.Repeat([]byte{0xff}, 9), 0x02)
	if _, _, err := readVarint(over); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("10th byte > 1 must overflow, got %v", err)
	}
}
