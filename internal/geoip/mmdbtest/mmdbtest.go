// Package mmdbtest writes tiny MaxMind DB files for tests.
package mmdbtest

import (
	"bytes"
	"net/netip"
	"sort"
)

// Build returns an IPv4 country database (record size 24) that maps each
// network ("2.0.0.0/16") to {"country": {"iso_code": code}}. Networks must not
// overlap.
func Build(networks map[string]string) []byte {
	type node struct {
		rec  [2]uint32
		kind [2]uint8 // 0 empty, 1 node, 2 data
	}
	nodes := []node{{}}
	var data bytes.Buffer
	offsets := map[string]uint32{}

	cidrs := make([]string, 0, len(networks))
	for c := range networks {
		cidrs = append(cidrs, c)
	}
	sort.Strings(cidrs)
	for _, cidr := range cidrs {
		code := networks[cidr]
		p := netip.MustParsePrefix(cidr).Masked()
		a := p.Addr().As4()
		off, ok := offsets[code]
		if !ok {
			off = uint32(data.Len())
			offsets[code] = off
			mapHeader(&data, 1)
			str(&data, "country")
			mapHeader(&data, 1)
			str(&data, "iso_code")
			str(&data, code)
		}
		cur := 0
		for i := 0; i < p.Bits(); i++ {
			bit := (a[i/8] >> (7 - i%8)) & 1
			if i == p.Bits()-1 {
				nodes[cur].kind[bit], nodes[cur].rec[bit] = 2, off
				break
			}
			if nodes[cur].kind[bit] != 1 {
				nodes = append(nodes, node{})
				nodes[cur].kind[bit], nodes[cur].rec[bit] = 1, uint32(len(nodes)-1)
			}
			cur = int(nodes[cur].rec[bit])
		}
	}

	var out bytes.Buffer
	count := uint32(len(nodes))
	for _, n := range nodes {
		for side := 0; side < 2; side++ {
			v := count
			switch n.kind[side] {
			case 1:
				v = n.rec[side]
			case 2:
				v = count + 16 + n.rec[side]
			}
			out.Write([]byte{byte(v >> 16), byte(v >> 8), byte(v)})
		}
	}
	out.Write(make([]byte, 16))
	out.Write(data.Bytes())

	out.WriteString("\xab\xcd\xefMaxMind.com")
	mapHeader(&out, 9)
	str(&out, "binary_format_major_version")
	uintv(&out, 5, 2)
	str(&out, "binary_format_minor_version")
	uintv(&out, 5, 0)
	str(&out, "build_epoch")
	uintv(&out, 9, 1757894400)
	str(&out, "database_type")
	str(&out, "DBIP-Country-Lite")
	str(&out, "description")
	mapHeader(&out, 1)
	str(&out, "en")
	str(&out, "test")
	str(&out, "ip_version")
	uintv(&out, 5, 4)
	str(&out, "languages")
	control(&out, 11, 1)
	str(&out, "en")
	str(&out, "node_count")
	uintv(&out, 6, uint64(count))
	str(&out, "record_size")
	uintv(&out, 5, 24)
	return out.Bytes()
}

// control writes a type/size control byte (sizes below 29 only).
func control(b *bytes.Buffer, typ, size int) {
	if typ <= 7 {
		b.WriteByte(byte(typ<<5 | size))
		return
	}
	b.WriteByte(byte(size))
	b.WriteByte(byte(typ - 7))
}

func str(b *bytes.Buffer, s string) {
	control(b, 2, len(s))
	b.WriteString(s)
}

func mapHeader(b *bytes.Buffer, pairs int) { control(b, 7, pairs) }

func uintv(b *bytes.Buffer, typ int, v uint64) {
	var digits []byte
	for ; v > 0; v >>= 8 {
		digits = append([]byte{byte(v)}, digits...)
	}
	control(b, typ, len(digits))
	b.Write(digits)
}
