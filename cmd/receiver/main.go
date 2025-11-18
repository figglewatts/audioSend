package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/cocoonlife/goalsa"
)

const (
	sampleRate    = 44100
	channels      = 2
	bytesPerFrame = 4 // 16-bit stereo: 2 bytes * 2 channels
)

func bytesToInt16Slice(src []byte, dst []int16) []int16 {
	samples := len(src) / 2
	if cap(dst) < samples {
		dst = make([]int16, samples)
	}
	dst = dst[:samples]
	for i := 0; i < samples; i++ {
		lo := uint16(src[2*i])
		hi := uint16(src[2*i+1])
		dst[i] = int16(hi<<8 | lo)
	}
	return dst
}

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("usage: %s <sender_ip:port>", os.Args[0])
	}
	senderAddr := os.Args[1]

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	conn, err := net.Dial("tcp", senderAddr)
	if err != nil {
		log.Fatalf("failed to connect to sender %s: %v", senderAddr, err)
	}
	defer conn.Close()
	log.Printf("connected to sender at %s", senderAddr)

	bufferParams := alsa.BufferParams{
		BufferFrames: sampleRate * 2,  // 2 seconds total
		PeriodFrames: sampleRate / 10, // 100 ms
		Periods:      20,              // 20 * 100ms = 2s
	}

	playDev, err := alsa.NewPlaybackDevice("default", channels, alsa.FormatS16LE, sampleRate, bufferParams)
	if err != nil {
		log.Fatalf("failed to open playback device: %v", err)
	}
	defer playDev.Close()

	log.Printf("playing %d Hz, %d-ch S16_LE to ALSA device %q",
		sampleRate, channels, "default")

	// channel to decouple network from ALSA
	audioCh := make(chan []byte, 512)

	// ALSA playback
	go func() {
		var sampleBuf []int16

		for {
			select {
			case <-ctx.Done():
				return
			case data, ok := <-audioCh:
				if !ok {
					return
				}
				sampleBuf = bytesToInt16Slice(data, sampleBuf)

				_, err := playDev.Write(sampleBuf)
				if err != nil {
					if errors.Is(err, alsa.ErrUnderrun) {
						log.Println("ALSA underrun, continuing")
						continue
					}
					log.Fatalf("ALSA write error: %v", err)
				}
			}
		}
	}()

	// read from TCP, cut into whole frames, push to audioCh
	readBuf := make([]byte, 4096)
	var leftover []byte

	for {
		select {
		case <-ctx.Done():
			close(audioCh)
			return
		default:
		}

		n, err := conn.Read(readBuf)
		if err != nil {
			if err == io.EOF {
				log.Println("sender closed connection")
				close(audioCh)
				return
			}
			log.Fatalf("read error: %v", err)
		}
		if n <= 0 {
			continue
		}

		chunk := readBuf[:n]
		data := append(leftover, chunk...)

		frames := len(data) / bytesPerFrame
		if frames == 0 {
			// not enough for a single frame yet
			leftover = data
			continue
		}

		byteCount := frames * bytesPerFrame
		audioBytes := data[:byteCount]
		leftover = data[byteCount:]

		// Copy before sending to channel (avoid reuse issues)
		bufCopy := make([]byte, len(audioBytes))
		copy(bufCopy, audioBytes)

		select {
		case audioCh <- bufCopy:
			// queued OK
		default:
			// buffer full – drop this packet rather than blocking
			log.Println("audio channel full on receiver, dropping packet")
		}
	}
}
