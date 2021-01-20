package yap

import (
	"fmt"
	"math"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// Request
type Request struct {
	id       int
	deadline int
	resolved string

	// Target is the endpoint where packets will be sent.
	Target string

	// Count tells pinger to stop after sending (and receiving) Count echo packets.
	Count int

	// Interval is the wait time between each packet send(in milliseconds).
	Interval int

	// Timeout specifies a timeout before the last packet to be sent(in milliseconds).
	Timeout int
}

func (r *Request) rid() string {
	return genid(r.id, r.resolved)
}

func (r *Request) setDefaults() {
	if r.Count <= 0 {
		r.Count = DefaultPingCount
	}

	if r.Interval <= 0 {
		r.Interval = DefaultPingInterval
	}

	if r.Timeout <= 0 {
		r.Timeout = DefaultTimeout
	}
}

func genid(a, b interface{}) string {
	return fmt.Sprintf("%v:%v", a, b)
}

func microsecond() float64 {
	return float64(time.Now().UnixNano()) / 1e6
}

// Response
type Response struct {
	// Target is the endpoint where packets have been sent.
	Target string

	// Error the error status after the request completion.
	Error error

	// PkgLoss is the percentage of packets lost.
	PkgLoss float64

	// RTTMax is the maximum round-trip time sent via this pinger.
	RTTMax float64

	// RTTMin is the minimum round-trip time sent via this pinger.
	RTTMin float64

	// RTTMean is the average round-trip time sent via this pinger.
	RTTMean float64
}

// String returns the formatted response stats string.
func (r Response) String() string {
	return fmt.Sprintf("Target: %s, PkgLoss: %v, RTTMin: %.5fms, RTTMean: %.5fms, RTTMax: %.5fms",
		r.Target,
		r.PkgLoss,
		r.RTTMin,
		r.RTTMean,
		r.RTTMax,
	)
}

type echoReq struct {
	id  int
	seq int
	t   float64
}

type echoRsp struct {
	id   uint16
	seq  uint16
	code uint8
	t    float64
}

type option struct {
	addr string
}

const (
	DefaultListenAddr   = "0.0.0.0"
	DefaultPingCount    = 3
	DefaultPingInterval = 0x0F
	DefaultTimeout      = 3000

	defaultFilter = "less 48 and icmp"
)

var defaultOpt = option{
	addr: DefaultListenAddr,
}

// Option
type Option func(*option)

// Pinger represents a packet sender/receiver.
type Pinger struct {
	opt *option

	conn        *icmp.PacketConn
	pcapHandler *pcap.Handle

	mutex   sync.Mutex
	counter int

	reqMutex sync.Mutex
	echoReqs map[string]echoReq
	rspMutex sync.Mutex
	echoRsps map[string]chan echoRsp

	pendingMutex sync.Mutex
	pending      map[string]*Request
	closing      bool
}

// WithListenerAddr sets the Listening address.
func WithListenerAddr(addr string) Option {
	return func(o *option) {
		o.addr = addr
	}
}

// NewPinger returns a new Pinger instance and serves the connection.
func NewPinger(options ...Option) (*Pinger, error) {
	o := defaultOpt
	pg := &Pinger{
		opt:      &o,
		echoReqs: make(map[string]echoReq),
		echoRsps: make(map[string]chan echoRsp),
		pending:  make(map[string]*Request),
	}

	for _, opt := range options {
		opt(pg.opt)
	}

	rand.Seed(time.Now().UnixNano())
	pg.counter = int(rand.Int31n(int32(math.MaxUint16)))

	conn, err := icmp.ListenPacket("ip4:icmp", pg.opt.addr)
	if err != nil {
		return nil, err
	}
	pg.conn = conn

	handle, err := pcap.OpenLive("any", 128, false, pcap.BlockForever)
	if err != nil {
		return nil, err
	}

	if err = handle.SetBPFFilter(defaultFilter); err != nil {
		return nil, err
	}
	pg.pcapHandler = handle

	go pg.serveConn()
	return pg, nil
}

// Close closes the pinger connection.
func (pg *Pinger) Close() error {
	err := pg.conn.Close()
	pg.pcapHandler.Close()

	pg.closing = true
	return err
}

// NumPending returns the number of pending requests.
func (pg *Pinger) NumPending() int {
	pg.pendingMutex.Lock()
	n := len(pg.pending)
	pg.pendingMutex.Unlock()
	return n
}

// CallMulti invokes Call with multiple requests.
func (pg *Pinger) CallMulti(requests ...Request) []Response {
	wg := sync.WaitGroup{}

	mutex := sync.Mutex{}
	data := make(map[string]Response)
	for _, request := range requests {
		wg.Add(1)
		go func(req Request) {
			defer wg.Done()

			r := pg.Call(req)
			mutex.Lock()
			r.Target = req.Target
			data[req.Target] = r
			mutex.Unlock()
		}(request)
	}
	wg.Wait()

	ret := make([]Response, 0, len(data))
	for _, request := range requests {
		ret = append(ret, data[request.Target])
	}

	return ret
}

// Call invokes the Sendto syscall to send packages, waits for it to complete.
func (pg *Pinger) Call(request Request) Response {
	req := &request
	req.setDefaults()

	dst, err := net.ResolveIPAddr("ip4", req.Target)
	if err != nil {
		return Response{Target: req.Target, Error: err}
	}

	pg.mutex.Lock()
	if pg.counter >= math.MaxUint16 {
		pg.counter = 0
	}
	pg.counter += 1
	req.id = pg.counter
	req.resolved = dst.IP.String()
	pg.mutex.Unlock()

	req.deadline = req.Count*req.Interval + req.Timeout

	pg.rspMutex.Lock()
	pg.echoRsps[req.rid()] = make(chan echoRsp, 1024)
	pg.rspMutex.Unlock()

	done := make(chan Response, 1)
	go pg.call(req, done)

	pg.pendingMutex.Lock()
	pg.pending[req.rid()] = req
	pg.pendingMutex.Unlock()

	for i := 0; i < req.Count; i++ {
		if pg.closing {
			break
		}

		// Echo or Echo Reply Message
		//    0                   1                   2                   3
		//    0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
		//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
		//   |     Type      |     Code      |          Checksum             |
		//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
		//   |           Identifier          |        Sequence Number        |
		//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
		//   |     Data ...
		//   +-+-+-+-+-
		//
		//   IP Fields:
		//
		//   Type:
		//      8 for echo message;
		//      0 for echo reply message.
		//
		//   Code:
		//      0
		//
		//   Checksum:
		//      The checksum is the 16-bit ones's complement of the one's
		//      complement sum of the ICMP message starting with the ICMP Type.
		//      For computing the checksum , the checksum field should be zero.
		//      If the total length is odd, the received data is padded with one
		//      octet of zeros for computing the checksum.  This checksum may be
		//      replaced in the future.
		//
		//   Identifier:
		//      If code = 0, an identifier to aid in matching echos and replies,
		//      may be zero.
		//
		//   Sequence Number
		//
		msg := icmp.Message{
			Type: ipv4.ICMPTypeEcho,
			Code: 0,
			Body: &icmp.Echo{ID: req.id, Seq: i, Data: []byte("echo")},
		}
		pg.reqMutex.Lock()
		pg.echoReqs[genid(req.id, i)] = echoReq{id: req.id, seq: i, t: microsecond()}
		pg.reqMutex.Unlock()

		// calculates checksum here
		bs, err := msg.Marshal(nil)
		if err != nil {
			continue
		}

		if _, err = pg.conn.WriteTo(bs, dst); err != nil {
			continue
		}

		time.Sleep(time.Millisecond * time.Duration(req.Interval))
	}

	r := <-done
	r.Target = req.Target

	return r
}

func (pg *Pinger) call(req *Request, done chan Response) {
	pg.rspMutex.Lock()
	r := pg.echoRsps[req.rid()]
	pg.rspMutex.Unlock()

	deadline := time.After(time.Millisecond * time.Duration(req.deadline))
	calc := make(map[string]float64)

loop:
	for {
		select {
		case <-deadline:
			break loop

		case item := <-r:
			if pg.closing {
				break loop
			}

			key := genid(item.id, item.seq)
			pg.reqMutex.Lock()
			calc[key] = item.t - pg.echoReqs[key].t
			delete(pg.echoReqs, key)
			pg.reqMutex.Unlock()

			if len(calc) == req.Count {
				break loop
			}
		}
	}

	best := math.MaxFloat64
	var loss, mean, worst float64
	for i := 0; i < req.Count; i++ {
		v, ok := calc[genid(req.id, i)]
		if !ok {
			loss += 1
			continue
		}

		if v > 1e6 {
			v = 0
		}

		if best > v {
			best = v
		}
		if worst < v {
			worst = v
		}

		mean += v
	}

	pg.rspMutex.Lock()
	delete(pg.echoRsps, req.rid())
	pg.rspMutex.Unlock()

	pg.pendingMutex.Lock()
	delete(pg.pending, req.rid())
	pg.pendingMutex.Unlock()

	pkgloss := loss / float64(req.Count)
	if pkgloss == 1 {
		worst = float64(req.Timeout)
		best = float64(req.Timeout)
		mean = float64(req.Timeout)
	} else {
		mean = mean / (float64(req.Count) - loss)
	}

	done <- Response{PkgLoss: pkgloss, RTTMean: mean, RTTMax: worst, RTTMin: best}
}

func (pg *Pinger) serveConn() {
	packetSource := gopacket.NewPacketSource(pg.pcapHandler, pg.pcapHandler.LinkType())
	packetSource.Lazy = true
	packetSource.NoCopy = true

	for packet := range packetSource.Packets() {
		icmpLayer := packet.Layer(layers.LayerTypeICMPv4)
		if icmpLayer == nil {
			continue
		}

		icmpPkg := icmpLayer.(*layers.ICMPv4)

		// Summary of Message Types
		//  0  Echo Reply
		//  3  Destination Unreachable
		//  4  Source Quench
		//  5  Redirect
		//  8  Echo
		// 11  Time Exceeded
		// 12  Parameter Problem
		// 13  Timestamp
		// 14  Timestamp Reply
		// 15  Information Request
		// 16  Information Reply
		if uint16(icmpPkg.TypeCode) != uint16(ipv4.ICMPTypeEchoReply) {
			continue
		}

		ipv4Layer := packet.Layer(layers.LayerTypeIPv4)
		if ipv4Layer == nil {
			continue
		}
		ipv4pkg := ipv4Layer.(*layers.IPv4)

		pg.rspMutex.Lock()
		r, ok := pg.echoRsps[genid(icmpPkg.Id, ipv4pkg.SrcIP.String())]
		if !ok {
			pg.rspMutex.Unlock()
			continue
		}

		r <- echoRsp{
			id:   icmpPkg.Id,
			seq:  icmpPkg.Seq,
			code: icmpPkg.TypeCode.Code(),
			t:    microsecond(),
		}
		pg.rspMutex.Unlock()
	}
}
