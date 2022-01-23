package t7

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/avast/retry-go"
	"github.com/k0kubun/go-ansi"
	"github.com/schollz/progressbar/v3"
)

func (t *Trionic) ReadECU(ctx context.Context, filename string) error {
	ok, err := t.KnockKnock(ctx)
	if err != nil || !ok {
		return fmt.Errorf("failed to authenticate: %v", err)
	}
	b, err := t.readECU(ctx, 0, 0x80000)
	if err != nil {
		return err
	}
	f, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("failed to create file: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return err
	}
	return nil
}

func (t *Trionic) readECU(ctx context.Context, addr, length int) ([]byte, error) {
	start := time.Now()
	var readPos int
	out := bytes.NewBuffer([]byte{})

	bar := progressbar.NewOptions(length,
		progressbar.OptionSetWriter(ansi.NewAnsiStdout()),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(20),
		progressbar.OptionSetDescription("[cyan][1/1][reset] dumping ECU"),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "[green]=[reset]",
			SaucerHead:    "[green]>[reset]",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}))

	defer bar.Finish()

	for readPos < length {
		bar.Set(out.Len())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			var readLength int
			if length-readPos >= 0xF5 {
				readLength = 0xF5
			} else {
				readLength = length - readPos
			}
			err := retry.Do(func() error {
				b, err := t.readMemoryByAddress(ctx, readPos, readLength)
				if err != nil {
					return err
				}
				out.Write(b)
				return nil
			},
				retry.Context(ctx),
				retry.OnRetry(func(n uint, err error) {
					log.Printf("#%d %v\n", n, err)
				}),
				retry.Attempts(5),
			)
			if err != nil {
				return nil, fmt.Errorf("failed to read memory by address, pos: 0x%X, length: 0x%X", readPos, readLength)
			}
			readPos += readLength
		}
	}
	bar.Set(out.Len())
	bar.Close()

	if err := t.endDownloadMode(ctx); err != nil {
		return nil, err
	}

	log.Println(" download done, took:", time.Since(start).Round(time.Second).String())

	return out.Bytes(), nil
}

func (t *Trionic) readMemoryByAddress(ctx context.Context, address, length int) ([]byte, error) {
	// Jump to read adress
	t.c.SendFrame(0x240, []byte{0x41, 0xA1, 0x08, 0x2C, 0xF0, 0x03, 0x00, byte(length)})
	t.c.SendFrame(0x240, []byte{0x00, 0xA1, byte((address >> 16) & 0xFF), byte((address >> 8) & 0xFF), byte(address & 0xFF), 0x00, 0x00, 0x00})

	f, err := t.c.Poll(ctx, t.defaultTimeout, 0x258)
	if err != nil {
		return nil, err
	}
	t.Ack(f.Data[0] & 0xBF)

	if f.Data[3] != 0x6C || f.Data[4] != 0xF0 {
		return nil, fmt.Errorf("failed to jump to 0x%X got response: %s", address, f.String())
	}

	t.c.SendFrame(0x240, []byte{0x40, 0xA1, 0x02, 0x21, 0xF0, 0x00, 0x00, 0x00}) // start data transfer
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	b, err := t.recvData(ctx, length)
	if err != nil {
		return nil, err
	}

	return b, nil
}

func (t *Trionic) recvData(ctx context.Context, length int) ([]byte, error) {
	var receivedBytes, payloadLeft int
	out := bytes.NewBuffer([]byte{})
outer:
	for receivedBytes < length {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			f, err := t.c.Poll(ctx, t.defaultTimeout, 0x258)
			if err != nil {
				return nil, err
			}
			if f.Data[0]&0x40 == 0x40 {
				payloadLeft = int(f.Data[2]) - 2 // subtract two non-payload bytes
				if payloadLeft > 0 && receivedBytes < length {
					out.WriteByte(f.Data[5])
					receivedBytes++
					payloadLeft--
				}
				if payloadLeft > 0 && receivedBytes < length {
					out.WriteByte(f.Data[6])
					receivedBytes++
					payloadLeft--
				}
				if payloadLeft > 0 && receivedBytes < length {
					out.WriteByte(f.Data[7])
					receivedBytes++
					payloadLeft--
				}
			} else {
				for i := 0; i < 6; i++ {
					if receivedBytes < length {
						out.WriteByte(f.Data[2+i])
						receivedBytes++
						payloadLeft--
						if payloadLeft == 0 {
							break
						}
					}
				}
			}
			t.Ack(f.Data[0] & 0xBF)
			if f.Data[0] == 0x80 || f.Data[0] == 0xC0 {
				break outer
			}
		}
	}
	return out.Bytes(), nil
}

func (t *Trionic) endDownloadMode(ctx context.Context) error {
	t.c.SendFrame(0x240, []byte{0x40, 0xA1, 0x01, 0x82, 0x00, 0x00, 0x00, 0x00})
	f, err := t.c.Poll(ctx, t.defaultTimeout, 0x258)
	if err != nil {
		return fmt.Errorf("end download mode: %v", err)
	}
	t.Ack(f.Data[0] & 0xBF)
	return nil
}