package cast

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
)

// dnsBuilder builds DNS messages for fixtures, with optional name compression.
type dnsBuilder struct{ b []byte }

func newResponse(an, ar int) *dnsBuilder {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[2:], 0x8400) // QR + AA
	binary.BigEndian.PutUint16(b[6:], uint16(an))
	binary.BigEndian.PutUint16(b[10:], uint16(ar))
	return &dnsBuilder{b: b}
}

// name writes labels followed by a compression pointer (ptr >= 0) or the root
// label, and returns the offset of the first label.
func (d *dnsBuilder) name(labels []string, ptr int) int {
	off := len(d.b)
	for _, l := range labels {
		d.b = append(d.b, byte(len(l)))
		d.b = append(d.b, l...)
	}
	if ptr >= 0 {
		d.b = binary.BigEndian.AppendUint16(d.b, 0xC000|uint16(ptr))
	} else {
		d.b = append(d.b, 0)
	}
	return off
}

// rr writes type/class/ttl and an rdata produced by fn (which appends to d.b).
func (d *dnsBuilder) rr(rtype uint16, class uint16, fn func()) {
	d.b = binary.BigEndian.AppendUint16(d.b, rtype)
	d.b = binary.BigEndian.AppendUint16(d.b, class)
	d.b = binary.BigEndian.AppendUint32(d.b, 120)
	lenOff := len(d.b)
	d.b = append(d.b, 0, 0)
	fn()
	binary.BigEndian.PutUint16(d.b[lenOff:], uint16(len(d.b)-lenOff-2))
}

func txtRData(kv ...string) []byte {
	var b []byte
	for _, s := range kv {
		b = append(b, byte(len(s)))
		b = append(b, s...)
	}
	return b
}

const (
	fixtureInstance = "Google-Nest-Hub-Max-e2a7ddaf331ad609f078033ff6293cda"
	fixtureHost     = "e2a7ddaf-331a-d609-f078-033ff6293cda"
)

// castResponseFixture mimics what a Nest Hub Max answers to a PTR query:
// PTR in the answer section, TXT/SRV/A in additionals, heavily compressed.
func castResponseFixture() []byte {
	d := newResponse(1, 4)
	svc := d.name([]string{"_googlecast", "_tcp", "local"}, -1)
	local := svc + 1 + len("_googlecast") + 1 + len("_tcp")
	var inst int
	d.rr(dnsTypePTR, dnsClassIN, func() { inst = d.name([]string{fixtureInstance}, svc) })

	d.name(nil, inst)
	d.rr(dnsTypeTXT, dnsClassIN|0x8000, func() {
		d.b = append(d.b, txtRData(
			"id=e2a7ddaf331ad609f078033ff6293cda",
			"cd=0123456789ABCDEF",
			"rm=",
			"ve=05",
			"md=Google Nest Hub Max",
			"ic=/setup/icon.png",
			"fn=Bedroom Display",
			"ca=231941",
			"st=0",
			"bs=FA8FCA000000",
			"nf=1",
			"rs=",
		)...)
	})

	d.name(nil, inst)
	var target int
	d.rr(dnsTypeSRV, dnsClassIN|0x8000, func() {
		d.b = append(d.b, 0, 0, 0, 0) // priority, weight
		d.b = binary.BigEndian.AppendUint16(d.b, 8009)
		target = d.name([]string{fixtureHost}, local)
	})

	d.name(nil, target)
	d.rr(dnsTypeA, dnsClassIN|0x8000, func() { d.b = append(d.b, 10, 9, 9, 249) })

	d.name(nil, target)
	d.rr(dnsTypeAAAA, dnsClassIN|0x8000, func() { d.b = append(d.b, make([]byte, 16)...) })
	return d.b
}

func TestBuildQuery(t *testing.T) {
	got := buildQuery(0, []dnsQuestion{{name: castService, qtype: dnsTypePTR}})
	want := []byte{
		0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, // header: 1 question
		11, '_', 'g', 'o', 'o', 'g', 'l', 'e', 'c', 'a', 's', 't',
		4, '_', 't', 'c', 'p',
		5, 'l', 'o', 'c', 'a', 'l',
		0,
		0, 12, // PTR
		0, 1, // IN
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("query = % x\nwant    % x", got, want)
	}
	qu := buildQuery(7, []dnsQuestion{{name: "a.local.", qtype: dnsTypeSRV, qu: true}})
	if binary.BigEndian.Uint16(qu) != 7 || binary.BigEndian.Uint16(qu[len(qu)-2:]) != 0x8001 {
		t.Fatalf("bad QU query % x", qu)
	}
}

func TestParseCastResponse(t *testing.T) {
	col := newCollector()
	if err := col.add(castResponseFixture(), net.IPv4(10, 9, 9, 1)); err != nil {
		t.Fatal(err)
	}
	want := []Device{{
		Name:  "Bedroom Display",
		Host:  "10.9.9.249", // from the A record, not the packet source
		UUID:  "e2a7ddaf331ad609f078033ff6293cda",
		Model: "Google Nest Hub Max",
		Port:  8009,
	}}
	if got := col.devices(); !reflect.DeepEqual(got, want) {
		t.Fatalf("devices = %+v\nwant %+v", got, want)
	}
	// Once everything is known, re-queries only ask for the PTR again.
	if qs := col.questions(); len(qs) != 1 || qs[0].qtype != dnsTypePTR {
		t.Fatalf("questions = %+v", qs)
	}
	// The same device seen again (e.g. via a second socket) is not duplicated.
	_ = col.add(castResponseFixture(), net.IPv4(10, 9, 9, 249))
	if got := col.devices(); len(got) != 1 {
		t.Fatalf("duplicate devices: %+v", got)
	}
}

func TestCollectorPartialAnswers(t *testing.T) {
	// PTR only: instance known but nothing to report yet; follow-up
	// questions ask for its SRV and TXT.
	d := newResponse(1, 0)
	svc := d.name([]string{"_googlecast", "_tcp", "local"}, -1)
	d.rr(dnsTypePTR, dnsClassIN, func() { d.name([]string{"Chromecast-abc"}, svc) })
	col := newCollector()
	if err := col.add(d.b, net.IPv4(192, 168, 1, 20)); err != nil {
		t.Fatal(err)
	}
	if devs := col.devices(); len(devs) != 0 {
		t.Fatalf("unexpected devices %+v", devs)
	}
	var types []uint16
	for _, q := range col.questions() {
		types = append(types, q.qtype)
		if q.qtype != dnsTypePTR && q.name != "Chromecast-abc._googlecast._tcp.local." {
			t.Fatalf("follow-up for wrong name %q", q.name)
		}
	}
	if !reflect.DeepEqual(types, []uint16{dnsTypePTR, dnsTypeSRV, dnsTypeTXT}) {
		t.Fatalf("question types %v", types)
	}

	// A TXT-only answer: host falls back to the packet source, port to 8009,
	// name to the instance label when fn is missing.
	d2 := newResponse(1, 0)
	d2.name([]string{"Chromecast-abc", "_googlecast", "_tcp", "local"}, -1)
	d2.rr(dnsTypeTXT, dnsClassIN, func() { d2.b = append(d2.b, txtRData("id=abc", "md=Chromecast")...) })
	if err := col.add(d2.b, net.IPv4(192, 168, 1, 20)); err != nil {
		t.Fatal(err)
	}
	want := []Device{{Name: "Chromecast-abc", Host: "192.168.1.20", UUID: "abc", Model: "Chromecast", Port: 8009}}
	if got := col.devices(); !reflect.DeepEqual(got, want) {
		t.Fatalf("devices = %+v, want %+v", got, want)
	}
}

func TestCollectorIgnoresUnrelated(t *testing.T) {
	col := newCollector()
	// Our own (or someone else's) query is not a response.
	if err := col.add(buildQuery(0, []dnsQuestion{{name: castService, qtype: dnsTypePTR}}), nil); err != nil {
		t.Fatal(err)
	}
	// Another service's PTR/TXT.
	d := newResponse(2, 0)
	svc := d.name([]string{"_airplay", "_tcp", "local"}, -1)
	var inst int
	d.rr(dnsTypePTR, dnsClassIN, func() { inst = d.name([]string{"TV"}, svc) })
	d.name(nil, inst)
	d.rr(dnsTypeTXT, dnsClassIN, func() { d.b = append(d.b, txtRData("id=x")...) })
	if err := col.add(d.b, net.IPv4(10, 0, 0, 2)); err != nil {
		t.Fatal(err)
	}
	if len(col.instances) != 0 || len(col.devices()) != 0 {
		t.Fatalf("unrelated records collected: %+v", col.instances)
	}
}

func TestDNSMalformed(t *testing.T) {
	fix := castResponseFixture()
	// Every truncation must be rejected or tolerated without panicking.
	for n := 0; n < len(fix); n++ {
		_ = newCollector().add(fix[:n], nil)
	}
	// Self-referencing compression pointer.
	loop := []byte{0xC0, 0x00}
	if _, _, err := readName(loop, 0); err == nil || !strings.Contains(err.Error(), "loop") {
		t.Fatalf("expected loop error, got %v", err)
	}
	// Reserved label type.
	if _, _, err := readName([]byte{0x80}, 0); err == nil {
		t.Fatal("expected bad label error")
	}
	if m := parseTXT([]byte{5, 'a', '='}); len(m) != 0 {
		t.Fatalf("truncated TXT parsed: %v", m)
	}
}

func TestCollectorCapsInstances(t *testing.T) {
	col := newCollector()
	for i := 0; i < maxInstances+10; i++ {
		col.instance(fmt.Sprintf("Chromecast-%d._googlecast._tcp.local.", i), nil)
	}
	if len(col.instances) != maxInstances {
		t.Fatalf("kept %d instances, want %d", len(col.instances), maxInstances)
	}
}

func TestCollectorCapsQueryAndAddrs(t *testing.T) {
	col := newCollector()
	long := strings.Repeat("x", 60)
	for i := range maxInstances {
		col.instance(fmt.Sprintf("%s-%d._googlecast._tcp.local.", long, i), nil)
	}
	qs := col.questions()
	if n := len(buildQuery(0, qs)); n > maxQuery || len(qs) < 10 {
		t.Fatalf("query of %d bytes with %d questions", n, len(qs))
	}
	if qs[0].qtype != dnsTypePTR {
		t.Fatal("PTR question dropped")
	}

	d := newResponse(maxAddrs+10, 0)
	for i := range maxAddrs + 10 {
		d.name([]string{fmt.Sprintf("h%d", i), "local"}, -1)
		d.rr(dnsTypeA, dnsClassIN, func() { d.b = append(d.b, 10, 0, byte(i>>8), byte(i)) })
	}
	if err := col.add(d.b, nil); err != nil {
		t.Fatal(err)
	}
	if len(col.addrs) != maxAddrs {
		t.Fatalf("kept %d addresses, want %d", len(col.addrs), maxAddrs)
	}
}
