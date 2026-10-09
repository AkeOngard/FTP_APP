package core

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestBlockSizeClamping(t *testing.T) {
	for in, want := range map[int]int{0: 1468, 100: 512, 512: 512, 1468: 1468, 8192: 8192, 65464: 65464, 1 << 20: 65464, -5: 512} {
		if got := (ConnectParams{BlockSize: in}).blockSize(); got != want {
			t.Errorf("blockSize(%d) = %d, want %d", in, got, want)
		}
	}
}

// Every block size must move the data intact, including sizes the server has to cap and servers that
// were never asked for a size.
func TestTFTPTransfersWithEveryBlockSize(t *testing.T) {
	m, params := startAllServers(t, nil)
	ctx := context.Background()
	data := randomBytes(t, 1_000_003) // not a multiple of any block size

	for _, blk := range []int{0, 512, 1468, 8192, 16384, 60000} {
		p := params[ProtoTFTP]
		p.BlockSize = blk
		c := mustConnect(t, m, p)
		name := "blk-" + itoaTest(blk) + ".bin"

		if err := c.Put(ctx, name, bytes.NewReader(data), int64(len(data)), nil); err != nil {
			t.Fatalf("block %d: Put: %v", blk, err)
		}
		var buf bytes.Buffer
		if err := c.Get(ctx, name, &buf, nil); err != nil || !bytes.Equal(buf.Bytes(), data) {
			t.Fatalf("block %d: Get differs: %v", blk, err)
		}
	}
}

// A larger block size has to make the transfer faster; this is the reason the option exists.
// The margin is deliberately wide so a busy machine cannot make it flaky.
func TestTFTPLargerBlocksAreFaster(t *testing.T) {
	m, params := startAllServers(t, nil)
	ctx := context.Background()
	data := randomBytes(t, 16<<20)

	timeIt := func(blk int) time.Duration {
		p := params[ProtoTFTP]
		p.BlockSize = blk
		c := mustConnect(t, m, p)
		var best time.Duration
		for i := 0; i < 2; i++ {
			s := time.Now()
			if err := c.Put(ctx, "speed.bin", bytes.NewReader(data), int64(len(data)), nil); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(s); best == 0 || d < best {
				best = d
			}
		}
		return best
	}
	slow, fast := timeIt(512), timeIt(8192)
	t.Logf("16 MB upload: 512-byte blocks %v, 8192-byte blocks %v", slow.Round(time.Millisecond), fast.Round(time.Millisecond))
	if fast*2 > slow {
		t.Errorf("8192-byte blocks (%v) should be at least twice as fast as 512-byte blocks (%v)", fast, slow)
	}
}

func itoaTest(n int) string { return fmtIntTest(n) }

func fmtIntTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
