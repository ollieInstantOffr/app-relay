// Package wire is the tunnel protocol between a gateway and a home Relay on
// top of a multiplexed session (internal/tunnel/mux).
//
// The gateway opens every stream. The first stream is the control stream:
// length-prefixed JSON messages in both directions, starting with Hello. Every
// other stream carries one client connection and starts with a StreamHeader
// written by the gateway. The home never trusts the header to choose a target:
// it looks the name or port up in its own published routes.
package wire

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
)

// Protocol versions spoken by this build.
const (
	Proto    = 1
	MinProto = 1
)

// Kind is what a data stream carries.
type Kind byte

const (
	KindHTTP  Kind = 1 // plain HTTP from the gateway's port 80
	KindHTTPS Kind = 2 // TLS from the gateway's port 443 (not decrypted)
	KindTCP   Kind = 3 // a published TCP stream port
)

func (k Kind) String() string {
	switch k {
	case KindHTTP:
		return "http"
	case KindHTTPS:
		return "https"
	case KindTCP:
		return "tcp"
	}
	return fmt.Sprintf("kind(%d)", byte(k))
}

// ---------------------------------------------------------------- stream header

var headerMagic = [2]byte{'R', 'T'}

const (
	headerVersion = 1
	// MaxNameLen bounds the SNI / Host name in a stream header.
	MaxNameLen = 255
)

// ErrBadHeader is returned for a malformed stream header.
var ErrBadHeader = errors.New("wire: malformed stream header")

// StreamHeader starts every data stream.
type StreamHeader struct {
	Kind Kind
	Port uint16         // public port on the gateway the client connected to
	Src  netip.AddrPort // client address as seen by the gateway
	Dst  netip.AddrPort // gateway address the client connected to
	Name string         // lowercase SNI (https) or Host without port (http); "" for tcp
}

// AppendStreamHeader appends the encoded header.
func AppendStreamHeader(b []byte, h StreamHeader) ([]byte, error) {
	if h.Kind < KindHTTP || h.Kind > KindTCP {
		return nil, fmt.Errorf("wire: invalid stream kind %d", h.Kind)
	}
	if len(h.Name) > MaxNameLen {
		return nil, fmt.Errorf("wire: name longer than %d bytes", MaxNameLen)
	}
	b = append(b, headerMagic[:]...)
	b = append(b, headerVersion, byte(h.Kind))
	b = binary.BigEndian.AppendUint16(b, h.Port)
	b = appendAddrPort(b, h.Src)
	b = appendAddrPort(b, h.Dst)
	b = append(b, byte(len(h.Name)))
	return append(b, h.Name...), nil
}

// WriteStreamHeader writes the header in one call.
func WriteStreamHeader(w io.Writer, h StreamHeader) error {
	b, err := AppendStreamHeader(make([]byte, 0, 64+len(h.Name)), h)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadStreamHeader reads a header without reading past it.
func ReadStreamHeader(r io.Reader) (StreamHeader, error) {
	var fixed [6]byte
	if _, err := io.ReadFull(r, fixed[:]); err != nil {
		return StreamHeader{}, err
	}
	if fixed[0] != headerMagic[0] || fixed[1] != headerMagic[1] || fixed[2] != headerVersion {
		return StreamHeader{}, ErrBadHeader
	}
	h := StreamHeader{Kind: Kind(fixed[3]), Port: binary.BigEndian.Uint16(fixed[4:6])}
	if h.Kind < KindHTTP || h.Kind > KindTCP {
		return StreamHeader{}, ErrBadHeader
	}
	var err error
	if h.Src, err = readAddrPort(r); err != nil {
		return StreamHeader{}, err
	}
	if h.Dst, err = readAddrPort(r); err != nil {
		return StreamHeader{}, err
	}
	var n [1]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return StreamHeader{}, err
	}
	name := make([]byte, n[0])
	if _, err := io.ReadFull(r, name); err != nil {
		return StreamHeader{}, err
	}
	h.Name = string(name)
	if h.Name != strings.ToLower(h.Name) || strings.ContainsAny(h.Name, " \t\r\n/") {
		return StreamHeader{}, ErrBadHeader
	}
	return h, nil
}

// Address encoding: family (0 none, 4, 6), address bytes, port.
func appendAddrPort(b []byte, ap netip.AddrPort) []byte {
	a := ap.Addr().Unmap()
	switch {
	case !a.IsValid():
		return append(b, 0)
	case a.Is4():
		b = append(b, 4)
		a4 := a.As4()
		b = append(b, a4[:]...)
	default:
		b = append(b, 6)
		a16 := a.As16()
		b = append(b, a16[:]...)
	}
	return binary.BigEndian.AppendUint16(b, ap.Port())
}

func readAddrPort(r io.Reader) (netip.AddrPort, error) {
	var fam [1]byte
	if _, err := io.ReadFull(r, fam[:]); err != nil {
		return netip.AddrPort{}, err
	}
	var a netip.Addr
	switch fam[0] {
	case 0:
		return netip.AddrPort{}, nil
	case 4:
		var b [4]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return netip.AddrPort{}, err
		}
		a = netip.AddrFrom4(b)
	case 6:
		var b [16]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return netip.AddrPort{}, err
		}
		a = netip.AddrFrom16(b).Unmap()
	default:
		return netip.AddrPort{}, ErrBadHeader
	}
	var p [2]byte
	if _, err := io.ReadFull(r, p[:]); err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(a, binary.BigEndian.Uint16(p[:])), nil
}

// ---------------------------------------------------------------- control messages

// MaxMessageLen bounds one control message.
const MaxMessageLen = 4 << 20

// Message is one control message; exactly one field is set. Unknown fields
// are ignored so newer peers can add messages.
type Message struct {
	Hello  *Hello     `json:"hello,omitempty"`
	Routes *Routes    `json:"routes,omitempty"`
	Ack    *RoutesAck `json:"routesAck,omitempty"`
	Ping   *Ping      `json:"ping,omitempty"`
	Pong   *Ping      `json:"pong,omitempty"`
	Stats  *Stats     `json:"stats,omitempty"`
	Unpair *Unpair    `json:"unpair,omitempty"`
}

// Hello is the first message on the control stream, sent by both sides.
type Hello struct {
	Proto    int      `json:"proto"`
	MinProto int      `json:"minProto"`
	Version  string   `json:"version"` // relay build version
	Features []string `json:"features,omitempty"`
	// Gateway only: its public addresses as far as it knows them.
	PublicIPs []string `json:"publicIps,omitempty"`
}

// Routes is the complete set of what the gateway should accept (home →
// gateway). Each push replaces the previous one.
type Routes struct {
	Generation uint64 `json:"generation"`
	// Names are published host names ("*.example.com" wildcards allowed),
	// accepted on ports 80 (Host) and 443 (SNI).
	Names []string `json:"names"`
	// TCP are published stream ports.
	TCP []uint16 `json:"tcp"`
}

// RoutesAck confirms a Routes generation (gateway → home).
type RoutesAck struct {
	Generation uint64      `json:"generation"`
	Errors     []PortError `json:"errors,omitempty"`
}

// PortError reports a port the gateway could not listen on.
type PortError struct {
	Port  uint16 `json:"port"`
	Error string `json:"error"`
}

// Ping measures round-trip time; the peer echoes it as Pong.
type Ping struct {
	ID     uint64 `json:"id"`
	SentNs int64  `json:"sentNs"`
}

// Stats are the gateway's counters since it started (gateway → home).
type Stats struct {
	ActiveConns  int64  `json:"activeConns"`
	Accepted     uint64 `json:"accepted"`
	RejectedName uint64 `json:"rejectedName"` // unknown SNI / Host
	RejectedPort uint64 `json:"rejectedPort"` // no session or unpublished port
	BytesIn      uint64 `json:"bytesIn"`      // client → home
	BytesOut     uint64 `json:"bytesOut"`     // home → client
}

// Unpair tells the gateway to forget the home (home → gateway).
type Unpair struct{}

// ErrMessageTooLarge is returned for control messages over MaxMessageLen.
var ErrMessageTooLarge = errors.New("wire: control message too large")

// WriteMessage writes one length-prefixed message.
func WriteMessage(w io.Writer, m *Message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(data) > MaxMessageLen {
		return ErrMessageTooLarge
	}
	b := make([]byte, 4, 4+len(data))
	binary.BigEndian.PutUint32(b, uint32(len(data)))
	_, err = w.Write(append(b, data...))
	return err
}

// ReadMessage reads one length-prefixed message.
func ReadMessage(r *bufio.Reader) (*Message, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size > MaxMessageLen {
		return nil, ErrMessageTooLarge
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("wire: control message: %w", err)
	}
	return &m, nil
}

// LocalHello is this build's Hello.
func LocalHello(version string) *Hello {
	return &Hello{Proto: Proto, MinProto: MinProto, Version: version}
}

// ErrIncompatible means the peers share no protocol version.
var ErrIncompatible = errors.New("wire: incompatible tunnel protocol versions")

// Negotiate returns the protocol version both hellos support.
func Negotiate(a, b *Hello) (int, error) {
	if a == nil || b == nil {
		return 0, ErrIncompatible
	}
	proto := min(a.Proto, b.Proto)
	if proto < max(a.MinProto, b.MinProto) || proto < 1 {
		return 0, fmt.Errorf("%w (local %d-%d, peer %d-%d)", ErrIncompatible, a.MinProto, a.Proto, b.MinProto, b.Proto)
	}
	return proto, nil
}

// ---------------------------------------------------------------- names

// Names matches server names like nginx server_name: exact names first, then
// the longest matching "*.suffix" wildcard (any depth).
type Names struct {
	exact map[string]bool
	wild  map[string]bool
}

// NewNames builds a matcher; names are lowercased, trimmed and may end in a dot.
func NewNames(names []string) *Names {
	n := &Names{exact: map[string]bool{}, wild: map[string]bool{}}
	for _, s := range names {
		s = NormalizeName(s)
		if s == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(s, "*."); ok {
			n.wild[rest] = true
		} else {
			n.exact[s] = true
		}
	}
	return n
}

// Match reports whether name (normalized by the caller or not) is published.
func (n *Names) Match(name string) bool {
	if n == nil {
		return false
	}
	name = NormalizeName(name)
	if name == "" {
		return false
	}
	if n.exact[name] {
		return true
	}
	for i := strings.IndexByte(name, '.'); i >= 0 && i < len(name)-1; {
		suffix := name[i+1:]
		if n.wild[suffix] {
			return true
		}
		j := strings.IndexByte(suffix, '.')
		if j < 0 {
			break
		}
		i += j + 1
	}
	return false
}

// NormalizeName lowercases and trims a name and strips one trailing dot.
func NormalizeName(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}
