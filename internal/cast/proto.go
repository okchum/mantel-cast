package cast

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Hand-rolled protobuf codec for the CastV2 CastMessage:
//
//	message CastMessage {
//	  required ProtocolVersion protocol_version = 1; // CASTV2_1_0 = 0
//	  required string source_id = 2;
//	  required string destination_id = 3;
//	  required string namespace = 4;
//	  required PayloadType payload_type = 5;        // STRING = 0, BINARY = 1
//	  optional string payload_utf8 = 6;
//	  optional bytes payload_binary = 7;
//	}

const (
	payloadString = 0
	payloadBinary = 1

	// maxFrameSize bounds a single frame. The Cast spec caps messages at
	// 64 KiB; allow some slack but refuse anything absurd.
	maxFrameSize = 1 << 20
)

type castMessage struct {
	ProtocolVersion uint64
	SourceID        string
	DestinationID   string
	Namespace       string
	PayloadType     uint64
	PayloadUTF8     string
	PayloadBinary   []byte
}

const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendTag(b []byte, field int, wire int) []byte {
	return appendVarint(b, uint64(field)<<3|uint64(wire))
}

func appendStringField(b []byte, field int, s string) []byte {
	b = appendTag(b, field, wireBytes)
	b = appendVarint(b, uint64(len(s)))
	return append(b, s...)
}

// marshal encodes m. Required fields are always written (proto2 semantics),
// even when they hold the zero value.
func (m *castMessage) marshal() []byte {
	n := 32 + len(m.SourceID) + len(m.DestinationID) + len(m.Namespace) + len(m.PayloadUTF8) + len(m.PayloadBinary)
	b := make([]byte, 0, n)
	b = appendTag(b, 1, wireVarint)
	b = appendVarint(b, m.ProtocolVersion)
	b = appendStringField(b, 2, m.SourceID)
	b = appendStringField(b, 3, m.DestinationID)
	b = appendStringField(b, 4, m.Namespace)
	b = appendTag(b, 5, wireVarint)
	b = appendVarint(b, m.PayloadType)
	if m.PayloadType == payloadBinary {
		b = appendTag(b, 7, wireBytes)
		b = appendVarint(b, uint64(len(m.PayloadBinary)))
		b = append(b, m.PayloadBinary...)
	} else {
		b = appendStringField(b, 6, m.PayloadUTF8)
	}
	return b
}

var errTruncated = errors.New("cast: truncated protobuf")

func readVarint(b []byte) (uint64, int, error) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		c := b[i]
		if i == 9 && c > 1 {
			// The 10th byte holds bit 63 only: anything more does not fit 64 bits.
			return 0, 0, errors.New("cast: varint overflow")
		}
		v |= uint64(c&0x7f) << (7 * uint(i))
		if c < 0x80 {
			return v, i + 1, nil
		}
	}
	if len(b) >= 10 {
		return 0, 0, errors.New("cast: varint overflow")
	}
	return 0, 0, errTruncated
}

// unmarshalCastMessage decodes a CastMessage, skipping unknown fields.
func unmarshalCastMessage(b []byte) (*castMessage, error) {
	m := &castMessage{}
	for len(b) > 0 {
		key, n, err := readVarint(b)
		if err != nil {
			return nil, err
		}
		b = b[n:]
		field, wire := int(key>>3), int(key&7)
		if field == 0 {
			return nil, errors.New("cast: invalid protobuf field 0")
		}
		switch wire {
		case wireVarint:
			v, n, err := readVarint(b)
			if err != nil {
				return nil, err
			}
			b = b[n:]
			switch field {
			case 1:
				m.ProtocolVersion = v
			case 5:
				m.PayloadType = v
			}
		case wireBytes:
			l, n, err := readVarint(b)
			if err != nil {
				return nil, err
			}
			b = b[n:]
			if l > uint64(len(b)) {
				return nil, errTruncated
			}
			data := b[:l]
			b = b[l:]
			switch field {
			case 2:
				m.SourceID = string(data)
			case 3:
				m.DestinationID = string(data)
			case 4:
				m.Namespace = string(data)
			case 6:
				m.PayloadUTF8 = string(data)
			case 7:
				m.PayloadBinary = append([]byte(nil), data...)
			}
		case wireFixed64:
			if len(b) < 8 {
				return nil, errTruncated
			}
			b = b[8:]
		case wireFixed32:
			if len(b) < 4 {
				return nil, errTruncated
			}
			b = b[4:]
		default:
			return nil, fmt.Errorf("cast: unsupported protobuf wire type %d", wire)
		}
	}
	return m, nil
}

// writeFrame writes m as a 4-byte big-endian length followed by the encoded
// message, in a single Write call.
func writeFrame(w io.Writer, m *castMessage) error {
	buf, err := encodeFrame(m)
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

// encodeFrame is m as a length-prefixed frame, or an error if it is too
// large to send.
func encodeFrame(m *castMessage) ([]byte, error) {
	body := m.marshal()
	if len(body) > maxFrameSize {
		return nil, fmt.Errorf("cast: message too large (%d bytes)", len(body))
	}
	buf := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	return append(buf, body...), nil
}

// readFrame reads one length-prefixed CastMessage.
func readFrame(r io.Reader) (*castMessage, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrameSize {
		return nil, fmt.Errorf("cast: frame too large (%d bytes)", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return unmarshalCastMessage(body)
}
