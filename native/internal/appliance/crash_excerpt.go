package appliance

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

const crashExcerptLimit = 32 << 10
const crashReadLimit = 4 << 20

// Samsung's panic handler prints every task and can bury the original fault.
// Keep the first fatal context as well as the tail, within the same USB budget.
func previousLogTail(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, crashReadLimit+1))
	if len(data) > crashReadLimit {
		data = data[:crashReadLimit]
		err = errors.Join(err, fmt.Errorf("previous log exceeds %d bytes; remainder unavailable", crashReadLimit))
	}
	if len(data) <= crashExcerptLimit {
		return data, err
	}
	first := -1
	for _, marker := range [][]byte{
		[]byte("Unable to handle kernel"), []byte("Internal error:"),
		[]byte("Kernel BUG at"), []byte("kernel BUG at"), []byte("BUG:"),
		[]byte("Kernel panic - not syncing"),
	} {
		if i := bytes.Index(data, marker); i >= 0 && (first < 0 || i < first) {
			first = i
		}
	}
	if first < 0 {
		return bytes.Clone(data[len(data)-crashExcerptLimit:]), err
	}
	start := max(0, first-4096)
	end := min(len(data), start+(24<<10))
	header := []byte(fmt.Sprintf("[first fatal context at input offset %d]\n", first))
	gap := []byte("\n[intermediate log omitted; recent tail follows]\n")
	room := crashExcerptLimit - len(header) - (end - start) - len(gap)
	tailStart := max(end, len(data)-room)
	out := make([]byte, 0, crashExcerptLimit)
	out = append(out, header...)
	out = append(out, data[start:end]...)
	if tailStart > end {
		out = append(out, gap...)
	}
	out = append(out, data[tailStart:]...)
	return out, err
}
