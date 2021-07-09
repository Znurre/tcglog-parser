// Copyright 2019 Canonical Ltd.
// Licensed under the LGPLv3 with static-linking exception.
// See LICENCE file for details.

package tcglog

import (
	"crypto"
	"fmt"
)

// EventData represents all event data types that appear in a log. Some implementations of this are exported so that event data
// contents can be inspected programatically.
//
// If an error is encountered when decoding the data associated with an event, the event data will implement the error interface
// which can be used for obtaining information about the decoding error.
type EventData interface {
	fmt.Stringer

	// Bytes is the raw event data bytes as they appear in the event log.
	Bytes() []byte
}

type rawEventData []byte

func (b rawEventData) Bytes() []byte {
	return []byte(b)
}

// invalidEventData corresponds to an event data blob that failed to decode correctly.
type invalidEventData struct {
	rawEventData
	err error
}

func (e *invalidEventData) String() string {
	return fmt.Sprintf("Invalid event data: %v", e.err)
}

func (e *invalidEventData) Error() string {
	return e.err.Error()
}

func (e *invalidEventData) Unwrap() error {
	return e.err
}

// opaqueEventData is event data whose format is unknown or implementation defined.
type opaqueEventData []byte

func (d opaqueEventData) Bytes() []byte {
	return []byte(d)
}

func (d opaqueEventData) String() string {
	return ""
}

// ComputeEventDigest computes the digest associated with the supplied event data bytes.
func ComputeEventDigest(alg crypto.Hash, data []byte) []byte {
	h := alg.New()
	h.Write(data)
	return h.Sum(nil)
}

func decodeEventData(data []byte, pcrIndex PCRIndex, eventType EventType, digests DigestMap, options *LogOptions) EventData {
	if options.EnableGrub && (pcrIndex == 8 || pcrIndex == 9) {
		if out := decodeEventDataGRUB(data, pcrIndex, eventType); out != nil {
			return out
		}
	}

	if options.EnableSystemdEFIStub && pcrIndex == options.SystemdEFIStubPCR {
		if out := decodeEventDataSystemdEFIStub(data, eventType); out != nil {
			return out
		}

	}

	out, err := decodeEventDataTCG(data, pcrIndex, eventType, digests)
	if err != nil {
		return &invalidEventData{rawEventData: data, err: err}
	}

	if out != nil {
		return out
	}

	return opaqueEventData(data)
}
