package cast

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// Device is a Cast device found by Discover.
type Device struct {
	Name, Host, UUID, Model string
	Port                    int
}

const (
	castService = "_googlecast._tcp.local."

	dnsTypeA    = 1
	dnsTypePTR  = 12
	dnsTypeTXT  = 16
	dnsTypeAAAA = 28
	dnsTypeSRV  = 33
	dnsClassIN  = 1

	// qu requests a unicast response (top bit of QCLASS, RFC 6762 §5.4).
	dnsClassQU = 0x8000
)

var mdnsGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// Discover sends mDNS queries for _googlecast._tcp.local and collects the
// answers until timeout (or ctx) expires. Queries are sent both from an
// ephemeral port (responders answer such "legacy unicast" queries directly)
// and, when port 5353 can be shared, from the mDNS port with the multicast
// group joined. It returns whatever devices were seen; an error is returned
// only if no socket could be opened, or ctx was cancelled before anything
// was found.
func Discover(ctx context.Context, timeout time.Duration) ([]Device, error) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	uc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, fmt.Errorf("cast: mdns listen: %w", err)
	}
	conns := []*net.UDPConn{uc}
	if mc, err := net.ListenMulticastUDP("udp4", nil, mdnsGroup); err == nil {
		conns = append(conns, mc)
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	type packet struct {
		data []byte
		src  net.IP
	}
	packets := make(chan packet, 64)
	for _, c := range conns {
		go func(c *net.UDPConn) {
			buf := make([]byte, 9000)
			for {
				n, addr, err := c.ReadFromUDP(buf)
				if err != nil {
					return
				}
				p := packet{data: append([]byte(nil), buf[:n]...), src: addr.IP}
				select {
				case packets <- p:
				case <-ctx.Done():
					return
				}
			}
		}(c)
	}

	col := newCollector()
	send := func() {
		q := buildQuery(0, col.questions())
		for _, c := range conns {
			_, _ = c.WriteToUDP(q, mdnsGroup)
		}
	}
	send()
	interval := 500 * time.Millisecond
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case p := <-packets:
			_ = col.add(p.data, p.src)
		case <-timer.C:
			send()
			interval *= 2
			timer.Reset(interval)
		case <-ctx.Done():
			devs := col.devices()
			if len(devs) == 0 && parent.Err() != nil {
				return nil, parent.Err()
			}
			return devs, nil
		}
	}
}

type dnsQuestion struct {
	name  string
	qtype uint16
	qu    bool
}

func appendName(b []byte, name string) []byte {
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if label == "" {
			continue
		}
		if len(label) > 63 {
			label = label[:63]
		}
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	return append(b, 0)
}

// buildQuery encodes an mDNS query message.
func buildQuery(id uint16, qs []dnsQuestion) []byte {
	b := make([]byte, 12, 64)
	binary.BigEndian.PutUint16(b[0:], id)
	binary.BigEndian.PutUint16(b[4:], uint16(len(qs)))
	for _, q := range qs {
		b = appendName(b, q.name)
		class := uint16(dnsClassIN)
		if q.qu {
			class |= dnsClassQU
		}
		b = binary.BigEndian.AppendUint16(b, q.qtype)
		b = binary.BigEndian.AppendUint16(b, class)
	}
	return b
}

var errDNSTruncated = errors.New("cast: truncated dns message")

// readName decodes a (possibly compressed) domain name at off and returns it
// with a trailing dot, plus the offset just past the name in the original
// position.
func readName(msg []byte, off int) (string, int, error) {
	var sb strings.Builder
	end := -1
	for jumps := 0; ; {
		if off >= len(msg) {
			return "", 0, errDNSTruncated
		}
		l := int(msg[off])
		switch {
		case l == 0:
			if end < 0 {
				end = off + 1
			}
			if sb.Len() == 0 {
				return ".", end, nil
			}
			return sb.String(), end, nil
		case l&0xC0 == 0xC0:
			if off+1 >= len(msg) {
				return "", 0, errDNSTruncated
			}
			if end < 0 {
				end = off + 2
			}
			jumps++
			if jumps > 32 {
				return "", 0, errors.New("cast: dns compression loop")
			}
			off = int(binary.BigEndian.Uint16(msg[off:]) & 0x3FFF)
		case l&0xC0 != 0:
			return "", 0, errors.New("cast: bad dns label")
		default:
			if off+1+l > len(msg) {
				return "", 0, errDNSTruncated
			}
			sb.Write(msg[off+1 : off+1+l])
			sb.WriteByte('.')
			if sb.Len() > 1024 {
				return "", 0, errors.New("cast: dns name too long")
			}
			off += 1 + l
		}
	}
}

type dnsRR struct {
	name  string
	rtype uint16
	off   int // offset of rdata in the message (for compressed names)
	data  []byte
}

// parseResponse returns all answer, authority and additional records of an
// mDNS response. Queries (QR bit clear) yield no records.
func parseResponse(msg []byte) ([]dnsRR, error) {
	if len(msg) < 12 {
		return nil, errDNSTruncated
	}
	if msg[2]&0x80 == 0 {
		return nil, nil // a query, not a response
	}
	qd := int(binary.BigEndian.Uint16(msg[4:]))
	rrCount := int(binary.BigEndian.Uint16(msg[6:])) + int(binary.BigEndian.Uint16(msg[8:])) + int(binary.BigEndian.Uint16(msg[10:]))
	off := 12
	for i := 0; i < qd; i++ {
		_, next, err := readName(msg, off)
		if err != nil {
			return nil, err
		}
		off = next + 4
		if off > len(msg) {
			return nil, errDNSTruncated
		}
	}
	var rrs []dnsRR
	for i := 0; i < rrCount; i++ {
		name, next, err := readName(msg, off)
		if err != nil {
			return rrs, err
		}
		off = next
		if off+10 > len(msg) {
			return rrs, errDNSTruncated
		}
		rtype := binary.BigEndian.Uint16(msg[off:])
		rdlen := int(binary.BigEndian.Uint16(msg[off+8:]))
		off += 10
		if off+rdlen > len(msg) {
			return rrs, errDNSTruncated
		}
		rrs = append(rrs, dnsRR{name: name, rtype: rtype, off: off, data: msg[off : off+rdlen]})
		off += rdlen
	}
	return rrs, nil
}

func parseTXT(data []byte) map[string]string {
	m := make(map[string]string)
	for len(data) > 0 {
		l := int(data[0])
		if 1+l > len(data) {
			break
		}
		s := string(data[1 : 1+l])
		data = data[1+l:]
		if s == "" {
			continue
		}
		k, v, _ := strings.Cut(s, "=")
		k = strings.ToLower(k)
		if _, dup := m[k]; !dup {
			m[k] = v
		}
	}
	return m
}

type mdnsInstance struct {
	name   string // full instance name, e.g. "Google-Nest-Hub-Max-xxxx._googlecast._tcp.local."
	target string // SRV target host name
	port   int
	hasSRV bool
	txt    map[string]string
	src    net.IP // source of the last packet describing this instance
}

type collector struct {
	instances map[string]*mdnsInstance // keyed by lower-cased instance name
	addrs     map[string]net.IP        // lower-cased host name -> IPv4
}

// maxInstances caps the Cast devices one discovery keeps track of, and
// maxAddrs the host addresses (A records may arrive before the SRV naming
// the host, so they are kept unasked).
const (
	maxInstances = 64
	maxAddrs     = 4 * maxInstances
)

// maxQuery keeps a query within one ordinary Ethernet frame; questions that
// do not fit are asked in a later round (map order varies, so all get asked).
const maxQuery = 1400

func newCollector() *collector {
	return &collector{instances: make(map[string]*mdnsInstance), addrs: make(map[string]net.IP)}
}

func isCastInstance(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), "."+castService)
}

func (c *collector) instance(name string, src net.IP) *mdnsInstance {
	key := strings.ToLower(name)
	in := c.instances[key]
	if in == nil {
		in = &mdnsInstance{name: name}
		// A LAN flooding PTR answers could otherwise grow the map (and the
		// follow-up query) without bound; extra names are parsed but dropped.
		if len(c.instances) < maxInstances {
			c.instances[key] = in
		}
	}
	if src != nil {
		in.src = src
	}
	return in
}

// add feeds one received packet into the collector.
func (c *collector) add(msg []byte, src net.IP) error {
	rrs, err := parseResponse(msg)
	for _, rr := range rrs {
		switch rr.rtype {
		case dnsTypePTR:
			if !strings.EqualFold(rr.name, castService) {
				continue
			}
			target, _, perr := readName(msg, rr.off)
			if perr == nil && isCastInstance(target) {
				c.instance(target, src)
			}
		case dnsTypeSRV:
			if !isCastInstance(rr.name) || len(rr.data) < 7 {
				continue
			}
			target, _, perr := readName(msg, rr.off+6)
			if perr != nil {
				continue
			}
			in := c.instance(rr.name, src)
			in.port = int(binary.BigEndian.Uint16(rr.data[4:]))
			in.target = target
			in.hasSRV = true
		case dnsTypeTXT:
			if !isCastInstance(rr.name) {
				continue
			}
			c.instance(rr.name, src).txt = parseTXT(rr.data)
		case dnsTypeA:
			if len(rr.data) == 4 && (len(c.addrs) < maxAddrs || c.addrs[strings.ToLower(rr.name)] != nil) {
				c.addrs[strings.ToLower(rr.name)] = net.IPv4(rr.data[0], rr.data[1], rr.data[2], rr.data[3])
			}
		}
	}
	return err
}

// questions returns the PTR question plus SRV/TXT/A follow-ups for
// instances whose details have not been seen yet.
func (c *collector) questions() []dnsQuestion {
	qs := []dnsQuestion{{name: castService, qtype: dnsTypePTR}}
	size := 12 + len(castService) + 2 + 4
	ask := func(name string, qtype uint16) bool {
		// A name encodes to at most len+2 bytes, plus type and class.
		if size += len(name) + 2 + 4; size > maxQuery {
			return false
		}
		qs = append(qs, dnsQuestion{name: name, qtype: qtype})
		return true
	}
	for _, in := range c.instances {
		ok := true
		if !in.hasSRV {
			ok = ask(in.name, dnsTypeSRV)
		} else if c.addrs[strings.ToLower(in.target)] == nil {
			ok = ask(in.target, dnsTypeA)
		}
		if ok && in.txt == nil {
			ok = ask(in.name, dnsTypeTXT)
		}
		if !ok {
			break
		}
	}
	return qs
}

// devices assembles the instances seen so far into Devices. An instance is
// reported once it has a TXT record (id/fn/md) or SRV record and an address.
func (c *collector) devices() []Device {
	var out []Device
	seen := make(map[string]bool)
	for _, in := range c.instances {
		if in.txt == nil && !in.hasSRV {
			continue
		}
		host := ""
		if ip := c.addrs[strings.ToLower(in.target)]; ip != nil {
			host = ip.String()
		} else if in.src != nil {
			host = in.src.String()
		}
		if host == "" {
			continue
		}
		d := Device{Host: host, Port: in.port}
		if d.Port == 0 {
			d.Port = defaultPort
		}
		if in.txt != nil {
			d.Name, d.Model, d.UUID = in.txt["fn"], in.txt["md"], in.txt["id"]
		}
		if d.Name == "" {
			d.Name, _, _ = strings.Cut(in.name, ".")
		}
		key := d.UUID
		if key == "" {
			key = strings.ToLower(in.name)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Host < out[j].Host
	})
	return out
}
