# yap

[![GoDoc](https://godoc.org/github.com/chenjiandongx/yap?status.svg)](https://godoc.org/github.com/chenjiandongx/yap)
[![Go Report Card](https://goreportcard.com/badge/github.com/chenjiandongx/yap)](https://goreportcard.com/report/github.com/chenjiandongx/yap)
[![License](https://img.shields.io/badge/License-MIT-brightgreen.svg)](https://opensource.org/licenses/MIT)

> **Y**et-**A**nother-**P**inger: A high-performance ICMP ping implementation build on top of BPF technology.

***yap*** uses the [gopacket](https://github.com/google/gopacket) library to receive and handle ICMP packets manually. ***gopacket*** provides a Go wrapper for ***libpcap*** written in C. gopacket is more than just a simple wrapper though. It provides additional functionality and takes advantage of Go things like interfaces, which makes it incredibly powerful.

***Libpcap*** is a portable open-source C/C++ library designed for Linux and Mac OS users which enables administrators to capture and filter packets. Packet sniffing tools like ***tcpdump*** use the libpcap format.

***Tcpdump*** is a command line utility that allows you to capture and analyze network traffic going through your system. It is often used to help troubleshoot network issues, as well as a security tool. One of tcpdump's most powerful features is its ability ***(BPF)*** to filter the captured packets using a variety of parameters, such as source and destination IP addresses, ports, protocols, etc.

**BPF (Berkeley Packet Filter)** offers a raw interface to data link layers, permitting raw link-layer packets to be sent and received. It supports filtering packets, allowing a userspace process to supply a filter program that specifies which packets it wants to receive. BPF returns only packets that pass the filter that the process supplies. This avoids copying unwanted packets from the operating system kernel to the process, greatly improving performance.

## Installation

Before we get started, you need to get libpcap installed first.

```shell
# yum
$ sudo yum install libpcap libpcap-devel

# apt-get
$ sudo apt-get install libpcap-dev

# OSX
$ brew install libpcap
```

Go get it.

```shell
$ go get -u github.com/chenjiandongx/yap
```

## Usages

### Request

```golang
// Request
type Request struct {
	// Target is the endpoint where packets will be sent.
	Target string

	// Count tells pinger to stop after sending (and receiving) Count echo packets.
	// Default: 3
	Count int

	// Interval is the wait time between each packet send(in milliseconds).
	// Default: 15
	Interval int

	// Timeout specifies a timeout before the last packet to be sent(in milliseconds).
	// Dafault: 3000
	Timeout int
}
```

### Response

```golang
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
```

### Example

```golang
// main.go
package main

import (
	"fmt"
	"time"

	"github.com/chenjiandongx/yap"
)

const (
	pingCount = 200
)

func main() {
	pg, err := yap.NewPinger()
	if err != nil {
		panic(err)
	}
	
	// don't forget to close in the end
	defer pg.Close()

	reqs := make([]yap.Request, 0)
	for _, host := range []string{"www.google.com", "www.huya.com"} {
		reqs = append(reqs, yap.Request{Target: host, Count: pingCount})
	}

	start := time.Now()

	// pg.Call / pg.CallMulti is thread-safety
	responses := pg.CallMulti(reqs...)
	for _, r := range responses {
		if r.Error != nil {
			panic(r.Error)
		}
		fmt.Println(r.String())
	}

	fmt.Printf("PING costs: %v\n", time.Since(start))
}
```

Output:
```shell
$ go run main.go 
Target: www.google.com, PkgLoss: 0, RTTMin: 169.72900ms, RTTMean: 246.97288ms, RTTMax: 449.19214ms
Target: www.huya.com, PkgLoss: 0, RTTMin: 21.06494ms, RTTMean: 104.33803ms, RTTMax: 433.38599ms

# (200 packets) * (15 interval) = 3000ms -> 3s
PING costs: 3.484154051s
```

## Performance

**The gopacket library makes it possible to build an asynchronous ICMP PING communication model.** yap utilizes a PacketConn to send data and then receives packets with a technology similar to ***tcpdump***. At the same time, BPF allows user-processes to reduce the number of packets processed, which will reduce the overhead of switching between kernel-space and user-space.

I have also written a general-purpose ping project before, [pinger](https://github.com/chenjiandongx/pinger), but it is based on the synchronous communication model, that is, the next packet will be sent only after the previous packet is received. In this scenario, the process is waiting for packets most of the time. 

What yap does is deriving two goroutines, one for sending data by PacketConn *(Sender)*, another for receiving data by gopacket *(Receiver)* as the work between them is independent of each other. The sender needn't wait for an RTT before it sends the next packet.

### pinger vs yap

**pinger:** ping a host 1000 times with 20 goroutines.
```golang
// pingertest/main.go
package main

import (
	"fmt"
	"time"

	"github.com/chenjiandongx/pinger"
)

const (
	PingCount    = 1000
	PingInterval = 10
	PingTimeout  = 3000
	Concurrency  = 20  // <- notice
)

func main() {
	opt := *pinger.DefaultICMPPingOpts
	opt.Interval = func() time.Duration { return time.Duration(PingInterval) * time.Millisecond }
	opt.PingTimeout = time.Duration(PingTimeout) * time.Millisecond
	opt.PingCount = PingCount
	opt.FailOver = 10
	opt.MaxConcurrency = Concurrency

	start := time.Now()
	stats, err := pinger.ICMPPing(&opt, []string{"www.huya.com"}...)
	if err != nil {
		panic(err)
	}

	for _, stat := range stats {
		fmt.Printf("Target: %s, PkgLoss: %v, RTTMin: %v, RTTMean: %v, RTTMax: %v\n", stat.Host, stat.PktLossRate, stat.Best, stat.Mean, stat.Mean)
	}
	fmt.Printf("PING Costs: %v\n", time.Since(start))
}

// Output:
// Target: www.huya.com, PkgLoss: 0.001, RTTMin: 14.312776ms, RTTMean: 16.549666ms, RTTMax: 16.549666ms
// PING Costs: 29.998770237s
```

**yap**: ping a host 1000 times only used 2 goroutines.
```golang
// yaptest/main.go
package main

import (
	"fmt"
	"time"

	"github.com/chenjiandongx/yap"
)

const (
	PingCount    = 1000
	PingInterval = 10
	PingTimeout  = 3000
)

func main() {
	pg, err := yap.NewPinger()
	if err != nil {
		panic(err)
	}
	defer pg.Close()

	start := time.Now()
	response := pg.Call(yap.Request{Target: "www.huya.com", Count: PingCount, Timeout: PingTimeout, Interval: PingInterval})
	if response.Error != nil {
		panic(response.Error)
	}

	fmt.Println(response.String())
	fmt.Printf("PING costs: %v\n", time.Since(start))
}

// Output:
// Target: www.huya.com, PkgLoss: 0.002, RTTMin: 20.06885ms, RTTMean: 21.96096ms, RTTMax: 95.51221ms
// PING costs: 13.012981437s
```

## FAQ

### Q: Does yap support Windows System?

No, cause to libpcap only for the Linux and MacOS users.

### Q: Why does yap need privileged mode or root?

All operating systems allow programs to create TCP or UDP sockets without requiring particular permissions. However, ping runs in ICMP (which is neither TCP nor UDP). This means we have to create raw IP packets and sniff the traffic on the network card. Operating systems are designed to require root for such operations.

This is because having unrestricted access to the NIC can expose the user to risks if the application running has bad intentions. This is not the case with yap of course, but nonetheless, we need this capability to create custom IP packets. Unfortunately, there is simply no other way to create ICMP packets.


## License

MIT [©chenjiandongx](https://github.com/chenjiandongx)
