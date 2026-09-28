// Package ambient implements automatic panel brightness without Android APIs.
package ambient

import (
	"encoding/binary"
	"fmt"
	"time"
)

// The supplied LightSensor::readEvents reads 26 packed bytes: signed lux/CCT,
// R/G/B/C int16, integration/gain bytes, then the uint64 timestamp at offset18.
// Lux is already computed by SSP; RGB counts must NOT be presented as lux.
const RecordSize = 26
const StaleAfter = 2 * time.Second
const MaxLux = 1_000_000 // Input sanity limit, not a claimed TMD/panel specification.
type Sample struct {
	Lux       int32
	Timestamp uint64
	Received  time.Time
}

func ParseSSP(b []byte, now time.Time) (Sample, error) {
	if len(b) != RecordSize {
		return Sample{}, fmt.Errorf("SSP light: expected 26 bytes, got %d", len(b))
	}
	s := Sample{Lux: int32(binary.LittleEndian.Uint32(b)), Timestamp: binary.LittleEndian.Uint64(b[18:]), Received: now}
	if s.Lux < 0 || s.Lux > MaxLux || s.Timestamp == 0 || s.Timestamp > 1<<63-1 {
		return Sample{}, fmt.Errorf("invalid SSP illuminance/timestamp")
	}
	return s, nil
}
