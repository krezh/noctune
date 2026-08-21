package audio

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// oggDemuxer extracts packets from an Ogg bitstream (RFC 3533) — just
// enough to pull the Opus packets ffmpeg encodes back out of its "-f ogg"
// output. It assumes a single logical bitstream (ffmpeg never multiplexes
// more than one) and doesn't verify page CRCs, since the input is our own
// local ffmpeg process, not untrusted network data.
type oggDemuxer struct {
	r *bufio.Reader

	queue   [][]byte // packets fully decoded from the most recent page(s)
	partial []byte   // bytes of a packet still being assembled across pages
}

func newOggDemuxer(r io.Reader) *oggDemuxer {
	return &oggDemuxer{r: bufio.NewReaderSize(r, 8192)}
}

// ReadPacket returns the next Opus packet, reading and demuxing further
// Ogg pages as needed. It returns io.EOF once the stream ends cleanly.
func (d *oggDemuxer) ReadPacket() ([]byte, error) {
	for len(d.queue) == 0 {
		if err := d.readPage(); err != nil {
			return nil, err
		}
	}
	pkt := d.queue[0]
	d.queue = d.queue[1:]
	return pkt, nil
}

const oggPageHeaderLen = 27

func (d *oggDemuxer) readPage() error {
	var hdr [oggPageHeaderLen]byte
	if _, err := io.ReadFull(d.r, hdr[:]); err != nil {
		return err
	}
	if string(hdr[0:4]) != "OggS" {
		return errors.New("ogg: bad capture pattern")
	}

	numSegments := int(hdr[26])
	segTable := make([]byte, numSegments)
	if _, err := io.ReadFull(d.r, segTable); err != nil {
		return fmt.Errorf("ogg: read segment table: %w", err)
	}

	total := 0
	for _, s := range segTable {
		total += int(s)
	}
	payload := make([]byte, total)
	if _, err := io.ReadFull(d.r, payload); err != nil {
		return fmt.Errorf("ogg: read page payload: %w", err)
	}

	offset := 0
	segStart := 0
	for i, s := range segTable {
		offset += int(s)
		switch {
		case s < 255:
			// A segment shorter than 255 always ends a packet.
			chunk := payload[segStart:offset]
			if len(d.partial) > 0 {
				d.partial = append(d.partial, chunk...)
				d.queue = append(d.queue, d.partial)
				d.partial = nil
			} else {
				d.queue = append(d.queue, append([]byte(nil), chunk...))
			}
			segStart = offset
		case i == len(segTable)-1:
			// A trailing 255-byte segment means the packet continues on
			// the next page.
			d.partial = append(d.partial, payload[segStart:offset]...)
			segStart = offset
		}
		// Else: a 255-byte segment mid-table just continues the packet
		// within this page — nothing to do yet.
	}
	return nil
}
