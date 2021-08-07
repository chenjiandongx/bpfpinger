# yap

[![GoDoc](https://godoc.org/github.com/chenjiandongx/yap?status.svg)](https://godoc.org/github.com/chenjiandongx/yap)
[![Go Report Card](https://goreportcard.com/badge/github.com/chenjiandongx/yap)](https://goreportcard.com/report/github.com/chenjiandongx/yap)
[![License](https://img.shields.io/badge/License-MIT-brightgreen.svg)](https://opensource.org/licenses/MIT)

> BPF 技术最初诞生就是为了高效地处理网络包。

BPF 利用其虚拟机技术，可以在一个比较靠前的位置处理网络包（cBPF 其实是在内核中过滤处理，相对 XDP 这种 eBPF 技术算靠后的了），减少从内核态到用户态的网络包的数量，**本质上也就是减少数据从内核态复制到用户态的开销以及两者上下文切换的开销**。

[yap](https://github.com/chenjiandongx/yap) 是一个 Golang 高性能的 ICMP PING 工具，其整体的实现思路是利用 `syscall.Sendto` 系统调用将自己封装好的 ICMP 包发送至网卡，然后再利用 [gopacket](https://github.com/google/gopacket) 库监听网卡并自己接收和处理 ICMP 包，这种设计模式使得 ICMP 的通信模式就变成了异步非阻塞的。


原生 ICMP 的同步通讯模型，在单个 goroutine 内，每一个 request 包需要等待上一个 reply 包到来才会继续发送，这也就导致了程序的大多数时间都需要等待一次 RTT（Round Trip Time）的时间。
<p align="center">
<img src="https://user-images.githubusercontent.com/19553554/107472251-94801880-6ba9-11eb-85c2-71b5497394ec.png" width="50%">
</br><i>图 1：同步阻塞模型</i>
</p>

yap 使用的异步通信模型，所有一个 ICMP 包只由全局唯一一个 goroutine 负责发送，然后使用 gopacket 监听网卡，将数据包进行处理和计算耗时，这样管理发送的 Sender 就可以持续不断的工作，无需同步地等待回包，大大提高了效率。
<p align="center">
<img src="https://user-images.githubusercontent.com/19553554/107473130-22103800-6bab-11eb-9a0e-31494bf5fcb0.png" width="50%">
</br><i>图 2：异步非阻塞模型</i>
</p>

### 优化细节

1）**更小的数据包**：icmp 包的 body 尽量的小。yap 使用的 ICMP 包整体大小约为 46 bytes，为什么是大约呢？因为在开发的过程中，我发现在 MacOS 上和在 CentOS 上使用同样的代码，最后计算的包的大小是不一样的，差了个 2 个 bytes。🤔 目前还不知道是操作系统本身实现不同导致的差异，还是因为我是开的虚拟机做开发，网卡虚拟化本身会导致的差异。

```golang
msg := icmp.Message{
	Type: ipv4.ICMPTypeEcho,
	Code: 0,
	Body: &icmp.Echo{ID: req.id, Seq: i, Data: []byte("yap")},
}
```

PS：这里补充一下 Echo Request 协议数据包描述。
```bash
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
```

2）**更严格的包过滤规则**：过滤包的规则越严格，从内核空间到用户空间的包就更少。当且仅当接收小于 48 bytes 的 icmp 的回显包。这样基本上接收到的所有包都是自己想要的了。

```golang
defaultFilter = "less 48 and icmp[icmptype] == icmp-echoreply"
```

不同的请求类型对应着不同的协议 ID
```bash
// Summary of Message Types

//  0  Echo Reply   <- notice
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
```

3）**更唯一的请求标识**：为了避免不同进程同时使用 yap 进行 ping 操作而导致的数据误差，yap 使用了随机初始化 Identifier + dstip 作为独立标识。最大程度上的降低数据误差的可能性。

```golang
// 随机初始化 counter
rand.Seed(time.Now().UnixNano())
pg.counter = int(rand.Int31n(int32(math.MaxUint16)))

// id+ + dstip 作为唯一标识
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
```

### 性能对比

> 对比实验操作系统：CentOS7

在写 yap 之前，我也曾经写过另外一个 ICMP ping 库，[pinger](https://github.com/chenjiandongx/pinger)，这个刚好就是上面所描述的同步模型的设计方案。所以就用这个库来跟 yap 做性能对比。

`/root/golang/src/pingtest/pinger/main.go`
```golang
package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"time"

	"github.com/chenjiandongx/pinger"
	"github.com/shirou/gopsutil/process"
)

const (
	PingCount    = 100000
	PingInterval = 50
	PingTimeout  = 3000
	Concurrency  = 20
)

func main() {
	go func() {
		if err := http.ListenAndServe("localhost:9999", nil); err != nil {
			panic(err)
		}
	}()

	proc, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		panic(err)
	}

	go func() {
		for {
			time.Sleep(3 * time.Second)
			busy, err := proc.CPUPercent()
			if err != nil {
				panic(err)
			}

			fmt.Println("pinger cpu.busy: ", busy)
		}
	}()

	opt := *pinger.DefaultICMPPingOpts
	opt.Interval = func() time.Duration { return time.Duration(PingInterval) * time.Millisecond }
	opt.PingTimeout = time.Duration(PingTimeout) * time.Millisecond
	opt.PingCount = PingCount
	opt.FailOver = 20
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
```

`/root/golang/src/pingtest/yap/main.go`
```golang
package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"time"

	"github.com/chenjiandongx/yap"
	"github.com/shirou/gopsutil/process"
)

const (
	PingCount    = 100000
	PingInterval = 50
	PingTimeout  = 3000
)

func main() {
	go func() {
		if err := http.ListenAndServe("localhost:8888", nil); err != nil {
			panic(err)
		}
	}()

	proc, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		panic(err)
	}

	go func() {
		for {
			time.Sleep(3 * time.Second)
			busy, err := proc.CPUPercent()
			if err != nil {
				panic(err)
			}

			fmt.Println("yap cpu.busy: ", busy)
		}
	}()


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
```

出于调试目的，我将两个进程都开启了 pprof 服务，分别暴露在 9999 和 8888 端口。接下来将两个程序同时跑起来。

![image](https://user-images.githubusercontent.com/19553554/107476803-8df59f00-6bb1-11eb-8e2e-6c0c4ec60b76.png)

可以看到，两者的 CPU 消耗是差不多的，约为 ~2%。

**但是**

既然是压测，那我们就需要模拟一下极端的环境，使用下面 bash 命令向 localhost 无情地不间断地发送 2000w 个 ICMP 包。
```shell
echo -n ">>>>>> start: ";date;time for i in {0..2000};do ping 127.0.0.1 -c 10000 -i0|awk '{print $7}'|awk -F '=' '{if($2>2) system("date");if($2>2) print $0 "ms"}';done
```

我们再看看这种极端网络环境下两者的 CPU 表现。

![image](https://user-images.githubusercontent.com/19553554/107477054-0fe5c800-6bb2-11eb-95ab-7571b47f6cb3.png)

**喔嚯，yap 进程依旧稳如老狗，而 pinger 进程的 CPU 使用率已经飙升到了 50% 以上....**

刚才讲到，为了调试我对两者均开启了 pprof 服务，那就来看看这段时间两个进程到底在干什么会产生如此大的性能差异。

#### pinger pprof

我悟了！进程在系统调用上花费了太多资源了，flat 高达 4.89s。

```bash
(pprof) top 20
Showing nodes accounting for 13.09s, 81.86% of 15.99s total
Dropped 140 nodes (cum <= 0.08s)
Showing top 20 nodes out of 84
      flat  flat%   sum%        cum   cum%
     4.89s 30.58% 30.58%      6.01s 37.59%  syscall.Syscall6
     2.63s 16.45% 47.03%      2.63s 16.45%  runtime.epollwait
     1.62s 10.13% 57.16%      1.62s 10.13%  runtime.futex
     0.76s  4.75% 61.91%      0.76s  4.75%  runtime.usleep
     0.51s  3.19% 65.10%      1.61s 10.07%  runtime.mallocgc
     0.34s  2.13% 67.23%      0.36s  2.25%  time.now
     0.32s  2.00% 69.23%      0.32s  2.00%  runtime.madvise
     0.26s  1.63% 70.86%      0.26s  1.63%  runtime.nextFreeFast
     0.23s  1.44% 72.30%      0.29s  1.81%  runtime.heapBitsSetType
     0.19s  1.19% 73.48%      3.41s 21.33%  runtime.findrunnable
     0.18s  1.13% 74.61%      0.18s  1.13%  runtime.casgstatus
     0.18s  1.13% 75.73%      0.18s  1.13%  runtime.memclrNoHeapPointers
     0.18s  1.13% 76.86%      1.51s  9.44%  runtime.newobject
     0.14s  0.88% 77.74%      0.34s  2.13%  runtime.mapaccess2
     0.14s  0.88% 78.61%      1.52s  9.51%  runtime.sysmon
     0.12s  0.75% 79.36%      7.55s 47.22%  net.(*IPConn).readFrom
     0.11s  0.69% 80.05%      0.17s  1.06%  time.Now
     0.10s  0.63% 80.68%      2.79s 17.45%  runtime.netpoll
     0.10s  0.63% 81.30%      0.86s  5.38%  runtime.reentersyscall
     0.09s  0.56% 81.86%      6.88s 43.03%  net.(*netFD).readFrom
```

我们知道，三层 IP 包传输在 Linux 对应的系统调用分别是 `syscall.Sendto` 和 `syscall.recvFrom`，接下来我们就验证一下上面的系统调用是不是主要耗在这两个方法上。

```bash
(pprof) peek syscall
Showing nodes accounting for 15.99s, 100% of 15.99s total
----------------------------------------------------------+-------------
      flat  flat%   sum%        cum   cum%   calls calls% + context
----------------------------------------------------------+-------------
                                             5.96s 99.17% |   syscall.recvfrom
                                             0.05s  0.83% |   syscall.sendto
     4.89s 30.58% 30.58%      6.01s 37.59%                | syscall.Syscall6
                                             0.90s 14.98% |   runtime.entersyscall
                                             0.22s  3.66% |   runtime.exitsyscall
----------------------------------------------------------+-------------
                                             0.86s   100% |   runtime.entersyscall
     0.10s  0.63% 31.21%      0.86s  5.38%                | runtime.reentersyscall
                                             0.67s 77.91% |   runtime.systemstack
                                             0.08s  9.30% |   runtime.casgstatus
                                             0.01s  1.16% |   runtime.save
```

这下就非常明显了吧，大多数的开销都在 `revcFrom` 系统调用上，因为我们刚才压测的时候往本地的网卡灌入了海量的 ICMP 包，**所以进程需要不断地陷入到内核态去将所有的这些 ICMP 包复制到用户态来进行验证处理。**

#### yap pprof

虽然 syscall 的开销也是占大头，但是进程总体的 CPU 开销是极小的（相比于上面的 pinger）。

```bash
(pprof) top 20
Showing nodes accounting for 210ms, 100% of 210ms total
Showing top 20 nodes out of 28
      flat  flat%   sum%        cum   cum%
     120ms 57.14% 57.14%      120ms 57.14%  runtime.usleep
      40ms 19.05% 76.19%       40ms 19.05%  runtime.cgocall
      30ms 14.29% 90.48%       30ms 14.29%  syscall.Syscall6
      10ms  4.76% 95.24%       10ms  4.76%  runtime.lock
      10ms  4.76%   100%      140ms 66.67%  runtime.sysmon
         0     0%   100%       30ms 14.29%  github.com/chenjiandongx/yap.(*Pinger).Call
         0     0%   100%       40ms 19.05%  github.com/google/gopacket.(*PacketSource).NextPacket
         0     0%   100%       40ms 19.05%  github.com/google/gopacket.(*PacketSource).packetsToChannel
         0     0%   100%       40ms 19.05%  github.com/google/gopacket/pcap.(*Handle).ReadPacketData
         0     0%   100%       40ms 19.05%  github.com/google/gopacket/pcap.(*Handle).getNextBufPtrLocked
         0     0%   100%       10ms  4.76%  github.com/google/gopacket/pcap.(*Handle).pcapNextPacketEx
         0     0%   100%       10ms  4.76%  github.com/google/gopacket/pcap.(*Handle).pcapNextPacketEx.func1
         0     0%   100%       30ms 14.29%  github.com/google/gopacket/pcap.(*Handle).waitForPacket
         0     0%   100%       30ms 14.29%  github.com/google/gopacket/pcap.(*Handle).waitForPacket.func1
         0     0%   100%       10ms  4.76%  github.com/google/gopacket/pcap._Cfunc_pcap_next_ex_escaping
         0     0%   100%       30ms 14.29%  github.com/google/gopacket/pcap._Cfunc_pcap_wait
         0     0%   100%       30ms 14.29%  golang.org/x/net/icmp.(*PacketConn).WriteTo
         0     0%   100%       30ms 14.29%  internal/poll.(*FD).WriteTo
         0     0%   100%       30ms 14.29%  main.main
         0     0%   100%       30ms 14.29%  net.(*IPConn).WriteTo
```

看下具体的系统调用情况，符合预期，主要都是 `sendto`，没有 `revcfrom`。

```bash
(pprof) peek syscall
Showing nodes accounting for 210ms, 100% of 210ms total
----------------------------------------------------------+-------------
      flat  flat%   sum%        cum   cum%   calls calls% + context
----------------------------------------------------------+-------------
                                              30ms   100% |   syscall.sendto
      30ms 14.29% 14.29%       30ms 14.29%                | syscall.Syscall6
----------------------------------------------------------+-------------
                                              30ms   100% |   internal/poll.(*FD).WriteTo
         0     0% 14.29%       30ms 14.29%                | syscall.Sendto
                                              30ms   100% |   syscall.sendto
```

#### 小结
1）yap 相比于 pinger 有着更优的执行效率，且性能受网络环境的影响极小，即使的同时收发海量的数据包，yap 的开销基本上是维持在一个常数。这本质上还是得益于 BPF 在内核态就将大量的数据包给过滤掉了，减小用户进程处理包的压力。

2）yap 的整体执行时间是可控的，它的异步模型并不需要同步等待回包，这也就意味着它的发包完全不受网络抖动的影响，而 pinger 如果再网络质量比较差的时候，即使多开 goroutine 也避免不了需要长时间等待 RTT 的尴尬局面。
