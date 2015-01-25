package utp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"strconv"
	"time"

	"bitbucket.org/anacrolix/sync"

	"github.com/bradfitz/iter"

	"github.com/anacrolix/jitter"

	"bitbucket.org/anacrolix/go.torrent/logonce"

	"github.com/spacemonkeygo/monotime"
)

type resolvedAddrStr string

type connKey struct {
	remoteAddr resolvedAddrStr
	connID     uint16
}

type Socket struct {
	mu          sync.Mutex
	event       sync.Cond
	pc          net.PacketConn
	conns       map[connKey]*Conn
	backlog     chan syn
	reads       chan read
	unusedReads chan read
	closing     chan struct{}

	ReadErr error
}

type read struct {
	data []byte
	from net.Addr
}

type syn struct {
	seq_nr, conn_id uint16
	addr            net.Addr
}

const (
	extensionTypeSelectiveAck = 1
)

type extensionField struct {
	Type  byte
	Bytes []byte
}

type header struct {
	Type          int
	Version       int
	ConnID        uint16
	Timestamp     uint32
	TimestampDiff uint32
	WndSize       uint32
	SeqNr         uint16
	AckNr         uint16
	Extensions    []extensionField
}

var logLevel = 0

func init() {
	logLevel, _ = strconv.Atoi(os.Getenv("GO_UTP_LOGGING"))
}

const (
	// Experimentation on localhost on OSX gives me this value. It appears to
	// be the largest approximate datagram size before remote libutp starts
	// selectively acking.
	minMTU     = 576
	recvWindow = 0x8000
	// Does not take into account possible extensions, since currently we
	// don't ever send any.
	maxHeaderSize  = 20
	maxPayloadSize = minMTU - maxHeaderSize
	maxRecvSize    = 0x2000
)

var (
	errClosed                   = errors.New("closed")
	errNotImplemented           = errors.New("not implemented")
	errTimeout        net.Error = timeoutError{"i/o timeout"}
)

type timeoutError struct {
	msg string
}

func (me timeoutError) Timeout() bool   { return true }
func (me timeoutError) Error() string   { return me.msg }
func (me timeoutError) Temporary() bool { return false }

func unmarshalExtensions(_type byte, b []byte) (n int, ef []extensionField, err error) {
	for _type != 0 {
		if _type != 1 {
			logonce.Stderr.Printf("utp extension %d", _type)
		}
		if len(b) < 2 || len(b) < int(b[1])+2 {
			err = fmt.Errorf("buffer ends prematurely: %x", b)
			return
		}
		ef = append(ef, extensionField{
			Type:  _type,
			Bytes: append([]byte{}, b[2:int(b[1])+2]...),
		})
		_type = b[0]
		n += 2 + int(b[1])
		b = b[2+int(b[1]):]
	}
	return
}

var errInvalidHeader = errors.New("invalid header")

func (h *header) Unmarshal(b []byte) (n int, err error) {
	// TODO: Are these endian-safe?
	h.Type = int(b[0] >> 4)
	h.Version = int(b[0] & 0xf)
	if h.Type > ST_MAX || h.Version != 1 {
		err = errInvalidHeader
		return
	}
	n, h.Extensions, err = unmarshalExtensions(b[1], b[20:])
	if err != nil {
		return
	}
	h.ConnID = binary.BigEndian.Uint16(b[2:4])
	h.Timestamp = binary.BigEndian.Uint32(b[4:8])
	h.TimestampDiff = binary.BigEndian.Uint32(b[8:12])
	h.WndSize = binary.BigEndian.Uint32(b[12:16])
	h.SeqNr = binary.BigEndian.Uint16(b[16:18])
	h.AckNr = binary.BigEndian.Uint16(b[18:20])
	n += 20
	return
}

func (h *header) Marshal() (p []byte) {
	if len(h.Extensions) != 0 {
		panic("marshalling of extensions not implemented")
	}
	p = make([]byte, 20)
	p[0] = byte(h.Type<<4 | 1)
	binary.BigEndian.PutUint16(p[2:4], h.ConnID)
	binary.BigEndian.PutUint32(p[4:8], h.Timestamp)
	binary.BigEndian.PutUint32(p[8:12], h.TimestampDiff)
	binary.BigEndian.PutUint32(p[12:16], h.WndSize)
	binary.BigEndian.PutUint16(p[16:18], h.SeqNr)
	binary.BigEndian.PutUint16(p[18:20], h.AckNr)
	return
}

var (
	_ net.Listener   = &Socket{}
	_ net.PacketConn = &Socket{}
)

const (
	csInvalid = iota
	csSynSent
	csConnected
	csGotFin
	csSentFin
	csDestroy
)

const (
	ST_DATA = iota
	ST_FIN
	ST_STATE
	ST_RESET
	ST_SYN

	ST_MAX = ST_SYN
)

type Conn struct {
	mu           sync.Mutex
	event        sync.Cond
	destroying   chan struct{}
	readDeadline time.Time

	recv_id, send_id uint16
	seq_nr, ack_nr   uint16
	lastAck          uint16
	lastTimeDiff     uint32
	peerWndSize      uint32

	readBuf []byte

	socket         net.PacketConn
	remoteAddr     net.Addr
	startTimestamp uint32

	cs  int
	err error

	unackedSends []send
	// Inbound payloads, the first is ack_nr+1.
	inbound []recv

	readDeadlineTimer *time.Timer
}

type send struct {
	acked       chan struct{}
	payloadSize uint32
	// This send was skipped in a selective ack.
	ackSkipped chan struct{}
	resend     func()
	timedOut   func()
}

func (s *send) Ack() {
	select {
	case <-s.acked:
	default:
		close(s.acked)
	}
}

type recv struct {
	seen bool
	data []byte
}

var (
	_ net.Conn = &Conn{}
)

func (c *Conn) timestamp() uint32 {
	return nowTimestamp() - c.startTimestamp
}

func (c *Conn) connected() bool {
	return c.cs == csConnected
}

func NewSocket(addr string) (s *Socket, err error) {
	s = &Socket{
		backlog:     make(chan syn, 5),
		reads:       make(chan read, 1),
		unusedReads: make(chan read, 100),
		closing:     make(chan struct{}),
	}
	s.event.L = &s.mu
	s.pc, err = net.ListenPacket("udp", addr)
	if err != nil {
		return
	}
	go s.reader()
	go s.dispatcher()
	return
}

func packetDebugString(h *header, payload []byte) string {
	return fmt.Sprintf("%#v: %q", h, payload)
}

func (s *Socket) reader() {
	defer close(s.reads)
	var b [maxRecvSize]byte
	for {
		if s.pc == nil {
			break
		}
		n, addr, err := s.pc.ReadFrom(b[:])
		if err != nil {
			select {
			case <-s.closing:
			default:
				s.ReadErr = err
			}
			return
		}
		var nilB []byte
		s.reads <- read{append(nilB, b[:n:n]...), addr}
	}
}

func (s *Socket) unusedRead(read read) {
	// log.Printf("unused read from %q", read.from.String())
	select {
	case s.unusedReads <- read:
	default:
	}
}

func (s *Socket) pushBacklog(syn syn) {
	for {
		select {
		case s.backlog <- syn:
			return
		default:
			select {
			case s.backlog <- syn:
				return
			case <-s.backlog:
			default:
				return
			}
		}
	}
}

func (s *Socket) dispatcher() {
	for {
		read, ok := <-s.reads
		if !ok {
			return
		}
		if len(read.data) < 20 {
			s.unusedRead(read)
			continue
		}
		b := read.data
		addr := read.from
		var h header
		hEnd, err := h.Unmarshal(b)
		if logLevel >= 1 {
			log.Printf("recvd utp msg: %s", packetDebugString(&h, b[hEnd:]))
		}
		if err != nil || h.Type > ST_MAX || h.Version != 1 {
			s.unusedRead(read)
			continue
		}
		s.mu.Lock()
		c, ok := s.conns[connKey{resolvedAddrStr(addr.String()), h.ConnID}]
		s.mu.Unlock()
		if ok {
			c.deliver(h, b[hEnd:])
			continue
		}
		if h.Type == ST_SYN {
			if logLevel >= 1 {
				log.Printf("adding SYN to backlog")
			}
			syn := syn{
				seq_nr:  h.SeqNr,
				conn_id: h.ConnID,
				addr:    addr,
			}
			s.pushBacklog(syn)
			continue
		}
		s.unusedRead(read)
	}
}

func Dial(addr string) (c *Conn, err error) {
	return DialTimeout(addr, 0)
}

func DialTimeout(addr string, timeout time.Duration) (c *Conn, err error) {
	s, err := NewSocket(":0")
	if err != nil {
		return
	}
	c, err = s.DialTimeout(addr, timeout)
	return

}

func (s *Socket) newConnID(remoteAddr resolvedAddrStr) (id uint16) {
	for {
		id = uint16(rand.Int())
		if _, ok := s.conns[connKey{remoteAddr, id + 1}]; !ok {
			return
		}
	}
}

func (s *Socket) newConn(addr net.Addr) (c *Conn) {
	c = &Conn{
		socket:         s.pc,
		remoteAddr:     addr,
		startTimestamp: nowTimestamp(),
		destroying:     make(chan struct{}),
	}
	c.event.L = &c.mu
	c.readDeadlineTimer = time.AfterFunc(-1, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.event.Broadcast()
	})
	return
}

func (s *Socket) Dial(addr string) (*Conn, error) {
	return s.DialTimeout(addr, 0)
}

func (s *Socket) DialTimeout(addr string, timeout time.Duration) (c *Conn, err error) {
	netAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return
	}
	c = s.newConn(netAddr)
	c.recv_id = s.newConnID(resolvedAddrStr(netAddr.String()))
	c.send_id = c.recv_id + 1
	if logLevel >= 1 {
		log.Printf("dial registering addr: %s", netAddr.String())
	}
	s.registerConn(c.recv_id, resolvedAddrStr(netAddr.String()), c)
	connErr := make(chan error, 1)
	go func() {
		connErr <- c.connect()
	}()
	var timeoutCh <-chan time.Time
	if timeout != 0 {
		timeoutCh = time.After(timeout)
	}
	select {
	case err = <-connErr:
	case <-timeoutCh:
		c.Close()
		err = errors.New("dial timeout")
	}
	return
}

func (c *Conn) wndSize() uint32 {
	var buffered int
	for _, r := range c.inbound {
		buffered += len(r.data)
	}
	buffered += len(c.readBuf)
	if buffered >= recvWindow {
		return 0
	}
	return recvWindow - uint32(buffered)
}

func nowTimestamp() uint32 {
	return uint32(monotime.Monotonic() / time.Microsecond)
}

func (c *Conn) send(_type int, connID uint16, payload []byte, seqNr uint16) (err error) {
	h := header{
		Type:          _type,
		Version:       1,
		ConnID:        connID,
		SeqNr:         seqNr,
		AckNr:         c.ack_nr,
		WndSize:       c.wndSize(),
		Timestamp:     c.timestamp(),
		TimestampDiff: c.lastTimeDiff,
	}
	p := h.Marshal()
	p = append(p, payload...)
	if logLevel >= 1 {
		log.Printf("writing utp msg to %s: %s", c.remoteAddr, packetDebugString(&h, payload))
	}
	n1, err := c.socket.WriteTo(p, c.remoteAddr)
	if err != nil {
		return
	}
	if n1 != len(p) {
		panic(n1)
	}
	return
}

// Retry with successfully longer timeouts, before destroying the connection
// after giving up.
func (s *send) timeoutResender() {
	// When to give up waiting for the last send to ack.
	deadline := time.Now()
	// Length of the next timeout.
	timeout := time.Second
	// Return true if we're acked, else return false when the last send times
	// out.
	wait := func() bool {
		deadline = deadline.Add(timeout)
		timeout *= 2
		d := deadline.Sub(time.Now())
		if d < 0 {
			// I've seen this happen when running with race detector, it is
			// probably due to the process struggling with CPU usage.
			log.Printf("deadline for UTP resend ack is in the past %s", -d)
			d = 0
		}
		select {
		case <-s.acked:
			return true
		case <-time.After(jitter.Duration(d, d/3)):
			return false
		}
	}
	// Each wait will be 1, 2, 4, 8, 16s.
	for range iter.N(5) {
		if wait() {
			return
		}
		s.resend()
	}
	// A last wait of 32s.
	if wait() {
		return
	}
	// At this point no ack has been received for 63s.
	s.timedOut()
}

// Retry after every 3 packets received for which ack was skipped.
func (s *send) ackSkippedResender() {
	skips := 0
	for {
		select {
		case <-s.ackSkipped:
		case <-s.acked:
			return
		}
		skips++
		// This was done every 3 skips, but that turned out to be an awful
		// lot.
		if skips != 3 {
			continue
		}
		if logLevel >= 1 {
			log.Printf("3 acks skipped, resending")
		}
		// Somone holding the Conn lock can be sending to ackSkipped, so this
		// is done asynchronously.
		go s.resend()
	}
}

func (c *Conn) write(_type int, connID uint16, payload []byte, seqNr uint16) (n int, err error) {
	if len(payload) > maxPayloadSize {
		payload = payload[:maxPayloadSize]
	}
	err = c.send(_type, connID, payload, seqNr)
	if err != nil {
		return
	}
	n = len(payload)
	if _type != ST_STATE {
		// Copy payload so caller to write can continue to use the buffer.
		payload = append([]byte{}, payload...)
		acked := make(chan struct{})
		ackSkipped := make(chan struct{})
		send := send{
			acked,
			uint32(len(payload)),
			ackSkipped,
			func() {
				c.mu.Lock()
				c.send(_type, connID, payload, seqNr)
				c.mu.Unlock()
			},
			func() {
				c.mu.Lock()
				c.destroy(errors.New("timed out waiting for ack"))
				c.mu.Unlock()
			},
		}
		c.unackedSends = append(c.unackedSends, send)
		go send.timeoutResender()
		go send.ackSkippedResender()
	}
	return
}

func (c *Conn) numUnackedSends() (num int) {
	for _, s := range c.unackedSends {
		select {
		case <-s.acked:
		default:
			num++
		}
	}
	return
}

func (c *Conn) cur_window() (window uint32) {
	for _, s := range c.unackedSends {
		select {
		case <-s.acked:
		default:
			window += s.payloadSize
		}
	}
	return
}

func (c *Conn) sendState() {
	c.write(ST_STATE, c.send_id, nil, c.seq_nr)
}

func seqLess(a, b uint16) bool {
	if b < 0x8000 {
		return a < b || a >= b-0x8000
	} else {
		return a < b && a >= b-0x8000
	}
}

// Ack our send with the given sequence number.
func (c *Conn) ack(nr uint16) {
	if !seqLess(c.lastAck, nr) {
		// Already acked.
		return
	}
	i := nr - c.lastAck - 1
	if int(i) >= len(c.unackedSends) {
		log.Printf("got ack ahead of syn (%x > %x)", nr, c.seq_nr-1)
		return
	}
	c.unackedSends[i].Ack()
	for {
		if len(c.unackedSends) == 0 {
			break
		}
		select {
		case <-c.unackedSends[0].acked:
		default:
			// Can't trim unacked sends any further.
			return
		}
		// Trim the front of the unacked sends.
		c.unackedSends = c.unackedSends[1:]
		c.lastAck++
	}
	c.event.Broadcast()
}

func (c *Conn) ackTo(nr uint16) {
	if !seqLess(nr, c.seq_nr) {
		return
	}
	for seqLess(c.lastAck, nr) {
		c.ack(c.lastAck + 1)
	}
}

type selectiveAckBitmask []byte

func (me selectiveAckBitmask) NumBits() int {
	return len(me) * 8
}

func (me selectiveAckBitmask) BitIsSet(index int) bool {
	return me[index/8]>>uint(index%8)&1 == 1
}

func (c *Conn) seqSend(seqNr uint16) *send {
	if !seqLess(c.lastAck, seqNr) {
		return nil
	}
	i := int(seqNr - c.lastAck - 1)
	if i >= len(c.unackedSends) {
		return nil
	}
	return &c.unackedSends[i]
}

func (c *Conn) ackSkipped(seqNr uint16) {
	send := c.seqSend(seqNr)
	if send == nil {
		return
	}
	select {
	case <-send.acked:
	case send.ackSkipped <- struct{}{}:
	}
	// log.Printf("ack skipped: %d", seqNr)
}

func (c *Conn) deliver(h header, payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer c.event.Broadcast()
	if h.ConnID != c.recv_id {
		panic("wrong conn id")
	}
	c.peerWndSize = h.WndSize
	c.ackTo(h.AckNr)
	for _, ext := range h.Extensions {
		switch ext.Type {
		case extensionTypeSelectiveAck:
			c.ackSkipped(h.AckNr + 1)
			bitmask := selectiveAckBitmask(ext.Bytes)
			for i := 0; i < bitmask.NumBits(); i++ {
				if bitmask.BitIsSet(i) {
					nr := h.AckNr + 2 + uint16(i)
					// log.Printf("selectively acked %d", nr)
					c.ack(nr)
				} else {
					c.ackSkipped(h.AckNr + 2 + uint16(i))
				}
			}
		}
	}
	if h.Timestamp == 0 {
		c.lastTimeDiff = 0
	} else {
		c.lastTimeDiff = c.timestamp() - h.Timestamp
	}
	// log.Printf("now micros: %d, header timestamp: %d, header diff: %d", c.timestamp(), h.Timestamp, h.TimestampDiff)
	if c.cs == csSynSent {
		if h.Type != ST_STATE {
			return
		}
		c.cs = csConnected
		c.ack_nr = h.SeqNr - 1
		return
	}
	if h.Type == ST_STATE {
		return
	}
	if !seqLess(c.ack_nr, h.SeqNr) {
		// Already received this packet.
		return
	}
	inboundIndex := int(h.SeqNr - c.ack_nr - 1)
	if inboundIndex < len(c.inbound) && c.inbound[inboundIndex].seen {
		// Already received this packet.
		return
	}
	// Extend inbound so the new packet has a place.
	for inboundIndex >= len(c.inbound) {
		c.inbound = append(c.inbound, recv{})
	}
	if inboundIndex != 0 {
		log.Printf("packet out of order, index=%d", inboundIndex)
	}
	c.inbound[inboundIndex] = recv{true, payload}
	// Consume consecutive next packets.
	for len(c.inbound) > 0 && c.inbound[0].seen {
		c.ack_nr++
		c.readBuf = append(c.readBuf, c.inbound[0].data...)
		c.inbound = c.inbound[1:]
	}
	c.sendState()
	if c.cs == csSentFin {
		if !seqLess(h.AckNr, c.seq_nr-1) {
			c.cs = csDestroy
		}
	}
	if h.Type == ST_FIN {
		// Skip csGotFin because we can't be missing any packets with the
		// current design.
		c.cs = csDestroy
	}
}

func (c *Conn) waitAck(seq uint16) {
	for {
		if c.cs == csDestroy {
			return
		}
		// TODO: Support being selectively acked.
		if !seqLess(c.lastAck, seq) {
			return
		}
		c.event.Wait()
	}
}

func (c *Conn) connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq_nr = 1
	_, err := c.write(ST_SYN, c.recv_id, nil, c.seq_nr)
	if err != nil {
		return err
	}
	c.cs = csSynSent
	if logLevel >= 2 {
		log.Printf("sent syn")
	}
	c.seq_nr++
	c.waitAck(1)
	c.event.Broadcast()
	return err
}

// Returns true if the connection was newly registered, false otherwise.
func (s *Socket) registerConn(recvID uint16, remoteAddr resolvedAddrStr, c *Conn) bool {
	if s.conns == nil {
		s.conns = make(map[connKey]*Conn)
	}
	key := connKey{remoteAddr, recvID}
	if _, ok := s.conns[key]; ok {
		return false
	}
	s.conns[key] = c
	return true
}

func (s *Socket) Accept() (c net.Conn, err error) {
	for {
		syn, ok := <-s.backlog
		if !ok {
			err = errClosed
			return
		}
		s.mu.Lock()
		_c := s.newConn(syn.addr)
		_c.send_id = syn.conn_id
		_c.recv_id = _c.send_id + 1
		_c.seq_nr = uint16(rand.Int())
		_c.lastAck = _c.seq_nr - 1
		_c.ack_nr = syn.seq_nr
		_c.cs = csConnected
		if !s.registerConn(_c.recv_id, resolvedAddrStr(syn.addr.String()), _c) {
			// SYN duplicates existing connection.
			s.mu.Unlock()
			continue
		}
		_c.sendState()
		// _c.seq_nr++
		c = _c
		s.mu.Unlock()
		return
	}
}

func (s *Socket) Addr() net.Addr {
	return s.pc.LocalAddr()
}

func (s *Socket) Close() (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closing:
	default:
		close(s.closing)
	}
	return
}

func (s *Socket) LocalAddr() net.Addr {
	return s.pc.LocalAddr()
}

func (s *Socket) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	read, ok := <-s.unusedReads
	if !ok {
		err = io.EOF
	}
	n = copy(p, read.data)
	addr = read.from
	return
}

func (s *Socket) SetDeadline(time.Time) error {
	return errNotImplemented
}

func (s *Socket) SetReadDeadline(time.Time) error {
	return errNotImplemented
}

func (s *Socket) SetWriteDeadline(time.Time) error {
	return errNotImplemented
}

func (s *Socket) WriteTo(b []byte, addr net.Addr) (int, error) {
	return s.pc.WriteTo(b, addr)
}

func (c *Conn) finish() {
	if c.cs != csConnected {
		return
	}
	finSeqNr := c.seq_nr
	c.write(ST_FIN, c.send_id, nil, finSeqNr)
	c.seq_nr++ // Spec says set to "eof_pkt".
	c.cs = csSentFin
	go func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.waitAck(finSeqNr)
		c.destroy(nil)
	}()
}

func (c *Conn) destroy(reason error) {
	if c.err != nil && reason != nil {
		log.Printf("duplicate destroy call: %s", reason)
	}
	if c.cs == csDestroy {
		return
	}
	c.cs = csDestroy
	c.err = reason
	c.event.Broadcast()
	close(c.destroying)
	for _, s := range c.unackedSends {
		s.Ack()
	}
}

func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finish()
	return nil
}

func (c *Conn) LocalAddr() net.Addr {
	return c.socket.LocalAddr()
}

func (c *Conn) Read(b []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if len(c.readBuf) != 0 {
			break
		}
		if c.cs == csDestroy || c.cs == csGotFin {
			err = c.err
			if err == nil {
				err = io.EOF
			}
			return
		}
		if !c.readDeadline.IsZero() && !time.Now().Before(c.readDeadline) {
			err = errTimeout
			return
		}
		if logLevel >= 2 {
			log.Printf("nothing to read, state=%d", c.cs)
		}
		c.event.Wait()
	}
	// log.Printf("read some data!")
	n = copy(b, c.readBuf)
	c.readBuf = c.readBuf[n:]

	return
}

func (c *Conn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

func (s *Conn) SetDeadline(time.Time) error {
	return errNotImplemented
}

func (c *Conn) SetReadDeadline(t time.Time) (err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	c.readDeadlineTimer.Reset(t.Sub(time.Now()))
	return
}

func (s *Conn) SetWriteDeadline(time.Time) error {
	return errNotImplemented
}

func (c *Conn) String() string {
	return fmt.Sprintf("<UTPConn %s-%s>", c.LocalAddr(), c.RemoteAddr())
}

func (c *Conn) Write(p []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(p) != 0 {
		if c.cs != csConnected {
			err = io.ErrClosedPipe
			return
		}
		for (c.cur_window() > c.peerWndSize || len(c.unackedSends) >= 0x400) && c.cs == csConnected {
			// log.Printf("cur_window: %d, wnd_size: %d, unacked sends: %d", c.cur_window(), c.peerWndSize, len(c.unackedSends))
			c.event.Wait()
		}
		var n1 int
		n1, err = c.write(ST_DATA, c.send_id, p, c.seq_nr)
		if err != nil {
			return
		}
		c.seq_nr++
		n += n1
		p = p[n1:]
	}
	return
}
