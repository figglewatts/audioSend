package main

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cocoonlife/goalsa"
)

const (
	sampleRate = 44100
	channels   = 2
	deviceName = "plughw:0,0"
)

// clientSet tracks connected TCP clients.
type clientSet struct {
	mu      sync.Mutex
	clients map[net.Conn]struct{}
}

func newClientSet() *clientSet {
	return &clientSet{
		clients: make(map[net.Conn]struct{}),
	}
}

func (cs *clientSet) add(c net.Conn) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.clients[c] = struct{}{}
	log.Printf("client connected: %s (total %d)", c.RemoteAddr(), len(cs.clients))
}

func (cs *clientSet) remove(c net.Conn) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if _, ok := cs.clients[c]; ok {
		delete(cs.clients, c)
		_ = c.Close()
		log.Printf("client disconnected: %s (total %d)", c.RemoteAddr(), len(cs.clients))
	}
}

func (cs *clientSet) broadcast(data []byte) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for c := range cs.clients {
		if _, err := c.Write(data); err != nil {
			log.Printf("write to %s failed: %v, removing client", c.RemoteAddr(), err)
			delete(cs.clients, c)
			_ = c.Close()
		}
	}
}

// int16SliceToBytes converts little-endian int16 samples to a []byte, reusing dst when possible.
func int16SliceToBytes(src []int16, dst []byte) []byte {
	if cap(dst) < len(src)*2 {
		dst = make([]byte, len(src)*2)
	}
	dst = dst[:len(src)*2]
	for i, v := range src {
		u := uint16(v)
		dst[2*i] = byte(u)        // low byte
		dst[2*i+1] = byte(u >> 8) // high byte
	}
	return dst
}

func main() {
	listenAddr := ":5000"
	if len(os.Args) > 1 {
		listenAddr = os.Args[1]
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// TCP listener for clients
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", listenAddr, err)
	}
	defer ln.Close()
	log.Printf("listening for clients on %s", listenAddr)

	cs := newClientSet()

	// accept loop in a goroutine
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				// if we're shutting down, or listener is closed, just exit the loop
				select {
				case <-ctx.Done():
					log.Println("accept loop exiting due to context cancel")
					return
				default:
				}
				if errors.Is(err, net.ErrClosed) {
					log.Println("listener closed, stopping accept loop")
					return
				}
				var ne net.Error
				if errors.As(err, &ne) && ne.Temporary() {
					log.Printf("temporary accept error: %v", err)
					time.Sleep(100 * time.Millisecond)
					continue
				}
				log.Printf("accept error: %v", err)
				return
			}

			cs.add(conn)
		}
	}()

	// ALSA capture setup with a fairly generous buffer to avoid overruns
	bufferParams := alsa.BufferParams{
		BufferFrames: sampleRate,
		PeriodFrames: sampleRate / 10,
		Periods:      10,
	}

	capDev, err := alsa.NewCaptureDevice(deviceName, channels, alsa.FormatS16LE, sampleRate, bufferParams)
	if err != nil {
		log.Fatalf("failed to open capture device %q: %v", deviceName, err)
	}
	defer capDev.Close()

	log.Printf("capturing %d Hz, %d-ch S16_LE from ALSA device %q", sampleRate, channels, deviceName)

	// channel to decouple ALSA capture from network broadcast
	audioCh := make(chan []byte, 32) // small in-memory ring buffer

	// take audio chunks from channel and send to clients
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case data, ok := <-audioCh:
				if !ok {
					return
				}
				cs.broadcast(data)
			}
		}
	}()

	// capture loop (runs in main goroutine)
	samplesPerPeriod := bufferParams.PeriodFrames * channels
	sampleBuf := make([]int16, samplesPerPeriod)
	byteBuf := make([]byte, samplesPerPeriod*2)

	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down sender")
			close(audioCh)
			ln.Close()
			return
		default:
		}

		n, err := capDev.Read(sampleBuf)
		if err != nil {
			if err == alsa.ErrOverrun {
				log.Println("ALSA overrun, continuing")
				continue
			}
			log.Fatalf("ALSA read error: %v", err)
		}
		if n <= 0 {
			continue
		}

		// n is number of int16 samples, not frames-per-channel.
		outBytes := int16SliceToBytes(sampleBuf[:n], byteBuf)

		// make a copy so the next ALSA read doesn't overwrite data
		bufCopy := make([]byte, len(outBytes))
		copy(bufCopy, outBytes)

		select {
		case audioCh <- bufCopy:
			// queued OK
		default:
			// channel full – drop this packet rather than blocking ALSA and causing overruns
			log.Println("audio channel full, dropping packet")
		}
	}
}
