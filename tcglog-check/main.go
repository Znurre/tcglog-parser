// Copyright 2019 Canonical Ltd.
// Licensed under the LGPLv3 with static-linking exception.
// See LICENCE file for details.

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/canonical/go-tpm2"
	"github.com/canonical/go-tpm2/mu"
	"github.com/canonical/tcglog-parser"
	"github.com/canonical/tcglog-parser/internal"
)

var (
	withGrub      bool
	withSdEfiStub bool
	sdEfiStubPcr  int
	noDefaultPcrs bool
	tpmPath       string
	logPath       string
	pcrs          internal.PCRArgList
)

type algorithmIdArgList tcglog.AlgorithmIdList

func (l *algorithmIdArgList) String() string {
	var builder bytes.Buffer
	for i, alg := range *l {
		if i > 0 {
			builder.WriteString(", ")
		}
		fmt.Fprintf(&builder, "%s", alg)
	}
	return builder.String()
}

func (l *algorithmIdArgList) Set(value string) error {
	algorithmId, err := internal.ParseAlgorithm(value)
	if err != nil {
		return err
	}
	*l = append(*l, algorithmId)
	return nil
}

func init() {
	flag.BoolVar(&withGrub, "with-grub", false, "Validate log entries made by GRUB in to PCR's 8 and 9")
	flag.BoolVar(&withSdEfiStub, "with-systemd-efi-stub", false, "Interpret measurements made by systemd's EFI stub Linux loader")
	flag.IntVar(&sdEfiStubPcr, "systemd-efi-stub-pcr", 8, "Specify the PCR that systemd's EFI stub Linux loader measures to")
	flag.BoolVar(&noDefaultPcrs, "no-default-pcrs", false, "Don't validate log entries for PCRs 0 - 7")
	flag.StringVar(&tpmPath, "tpm-path", "/dev/tpm0", "Validate log entries associated with the specified TPM")
	flag.StringVar(&logPath, "log-path", "", "Specify the path to the event log. The default path associated with the TPM "+
		"device is used if empty. If supplied, log entries are not validated with a TPM")
	flag.Var(&pcrs, "pcrs", "Validate log entries for the specified PCRs. Can be specified multiple times")
}

type efiBootVariableBehaviour int

const (
	efiBootVariableBehaviourUnknown efiBootVariableBehaviour = iota
	efiBootVariableBehaviourFull
	efiBootVariableBehaviourVarDataOnly
)

func pcrIndexListToSelect(l []tcglog.PCRIndex) (out tpm2.PCRSelect) {
	for _, i := range l {
		out = append(out, int(i))
	}
	return
}

func readPCRsFromTPM2Device(tpm *tpm2.TPMContext, algorithms tcglog.AlgorithmIdList) (map[tcglog.PCRIndex]tcglog.DigestMap, error) {
	result := make(map[tcglog.PCRIndex]tcglog.DigestMap)

	var selections tpm2.PCRSelectionList
	for _, alg := range algorithms {
		selections = append(selections, tpm2.PCRSelection{Hash: tpm2.HashAlgorithmId(alg), Select: pcrIndexListToSelect(pcrs)})
	}

	for _, i := range pcrs {
		result[i] = tcglog.DigestMap{}
	}

	_, digests, err := tpm.PCRRead(selections)
	if err != nil {
		return nil, fmt.Errorf("cannot read PCR values: %v", err)
	}

	for _, s := range selections {
		for _, i := range s.Select {
			result[tcglog.PCRIndex(i)][tcglog.AlgorithmId(s.Hash)] = tcglog.Digest(digests[s.Hash][i])
		}
	}
	return result, nil
}

func readPCRsFromTPM1Device(tpm *tpm2.TPMContext) (map[tcglog.PCRIndex]tcglog.DigestMap, error) {
	result := make(map[tcglog.PCRIndex]tcglog.DigestMap)
	for _, i := range pcrs {
		in, err := mu.MarshalToBytes(uint32(i))
		if err != nil {
			return nil, fmt.Errorf("cannot read PCR values due to a marshalling error: %v", err)
		}
		rc, _, out, err := tpm.RunCommandBytes(tpm2.StructTag(0x00c1), tpm2.CommandCode(0x00000015), in)
		if err != nil {
			return nil, fmt.Errorf("cannot read PCR values: %v", err)
		}
		if rc != tpm2.Success {
			return nil, fmt.Errorf("cannot read PCR values: unexpected response code (0x%08x)", rc)
		}
		result[i] = tcglog.DigestMap{}
		result[i][tcglog.AlgorithmSha1] = out
	}
	return result, nil
}

func getTPMDeviceVersion(tpm *tpm2.TPMContext) int {
	if isTpm2, _ := tpm.IsTPM2(); isTpm2 {
		return 2
	}

	payload, _ := mu.MarshalToBytes(uint32(0x00000005), uint32(4), uint32(0x00000103))
	if rc, _, _, err := tpm.RunCommandBytes(tpm2.StructTag(0x00c1), tpm2.CommandCode(0x00000065), payload); err == nil && rc == tpm2.Success {
		return 1
	}

	return 0
}

func readPCRs(algorithms tcglog.AlgorithmIdList) (map[tcglog.PCRIndex]tcglog.DigestMap, error) {
	tcti, err := tpm2.OpenTPMDevice(tpmPath)
	if err != nil {
		return nil, fmt.Errorf("could not open TPM device: %v", err)
	}
	tpm, _ := tpm2.NewTPMContext(tcti)
	defer tpm.Close()

	switch getTPMDeviceVersion(tpm) {
	case 2:
		return readPCRsFromTPM2Device(tpm, algorithms)
	case 1:
		return readPCRsFromTPM1Device(tpm)
	}

	return nil, errors.New("not a valid TPM device")
}

type incorrectDigestValue struct {
	algorithm tcglog.AlgorithmId
	expected  tcglog.Digest
}

type checkedEvent struct {
	*tcglog.Event
	measuredBytes         []byte
	measuredTrailingBytes []byte
	incorrectDigestValues []incorrectDigestValue
}

func (e *checkedEvent) extendsPCR() bool {
	if e.EventType == tcglog.EventTypeNoAction {
		return false
	}
	return true
}

func (e *checkedEvent) expectedMeasuredBytes(efiBootVariableQuirk bool) []byte {
	if err := e.dataDecoderErr(); err != nil {
		return nil
	}

	switch e.EventType {
	case tcglog.EventTypeEventTag, tcglog.EventTypeSCRTMVersion, tcglog.EventTypePlatformConfigFlags, tcglog.EventTypeTableOfDevices, tcglog.EventTypeNonhostInfo, tcglog.EventTypeOmitBootDeviceEvents:
		return e.Data.Bytes()
	case tcglog.EventTypeSeparator:
		if e.Data.(*tcglog.SeparatorEventData).IsError {
			var d [4]byte
			binary.LittleEndian.PutUint32(d[:], tcglog.SeparatorEventErrorValue)
			return d[:]
		}
		return e.Data.Bytes()
	case tcglog.EventTypeAction, tcglog.EventTypeEFIAction:
		return e.Data.Bytes()
	case tcglog.EventTypeEFIVariableDriverConfig, tcglog.EventTypeEFIVariableBoot, tcglog.EventTypeEFIVariableAuthority:
		if e.EventType == tcglog.EventTypeEFIVariableBoot && efiBootVariableQuirk {
			return e.Data.(*tcglog.EFIVariableData).VariableData
		}
		return e.Data.Bytes()
	case tcglog.EventTypeEFIGPTEvent:
		return e.Data.Bytes()
	case tcglog.EventTypeIPL:
		switch d := e.Data.(type) {
		case *tcglog.GrubStringEventData:
			var b bytes.Buffer
			d.EncodeMeasuredBytes(&b)
			return b.Bytes()
		case *tcglog.SystemdEFIStubEventData:
			var b bytes.Buffer
			d.EncodeMeasuredBytes(&b)
			return b.Bytes()
		}
	}

	return nil
}

func (e *checkedEvent) dataDecoderErr() error {
	if err, isErr := e.Data.(error); isErr {
		return err
	}
	return nil
}

func (e *checkedEvent) expectedDigest(alg tcglog.AlgorithmId) []byte {
	h := alg.GetHash().New()
	h.Write(e.measuredBytes)
	return h.Sum(nil)
}

func (e *checkedEvent) hasExpectedDigest(alg tcglog.AlgorithmId) bool {
	h := alg.GetHash().New()
	h.Write(e.measuredBytes)
	return bytes.Equal(e.Digests[alg], e.expectedDigest(alg))
}

func checkEvent(event *tcglog.Event, c *logChecker) (out *checkedEvent) {
	out = &checkedEvent{Event: event}

	for alg := range out.Digests {
		if len(out.measuredBytes) > 0 {
			// We've already determined the bytes measured for this event for a previous digest
			if !out.hasExpectedDigest(alg) {
				out.incorrectDigestValues = append(out.incorrectDigestValues, incorrectDigestValue{algorithm: alg, expected: out.expectedDigest(alg)})
			}
			continue
		}

		efiBootVariableBehaviourTry := c.efiBootVariableBehaviour

	Loop:
		for {
			// Determine what we expect to be measured
			out.measuredBytes = out.expectedMeasuredBytes(efiBootVariableBehaviourTry == efiBootVariableBehaviourVarDataOnly)
			if out.measuredBytes == nil {
				return
			}

			if m, ok := out.Data.(interface{ TrailingBytes() []byte }); ok {
				out.measuredTrailingBytes = m.TrailingBytes()
			}

			for {
				// Determine whether the digest is consistent with the current provisional measured bytes
				switch {
				case out.hasExpectedDigest(alg):
					// All good
					if out.EventType == tcglog.EventTypeEFIVariableBoot && c.efiBootVariableBehaviour == efiBootVariableBehaviourUnknown {
						// This is the first EV_EFI_VARIABLE_BOOT event, so record the measurement behaviour.
						c.efiBootVariableBehaviour = efiBootVariableBehaviourTry
						if efiBootVariableBehaviourTry == efiBootVariableBehaviourUnknown {
							c.efiBootVariableBehaviour = efiBootVariableBehaviourFull
						}
					}
					return
				case len(out.measuredTrailingBytes) > 0:
					// Invalid digest, the event data decoder determined there were trailing bytes, and we were expecting the measured
					// bytes to match the event data. Test if any of the trailing bytes only appear in the event data by truncating
					// the provisional measured bytes one byte at a time and re-testing.
					out.measuredBytes = out.measuredBytes[0 : len(out.measuredBytes)-1]
					out.measuredTrailingBytes = out.measuredTrailingBytes[0 : len(out.measuredTrailingBytes)-1]
				default:
					// Invalid digest
					if out.EventType == tcglog.EventTypeEFIVariableBoot && efiBootVariableBehaviourTry == efiBootVariableBehaviourUnknown {
						// This is the first EV_EFI_VARIABLE_BOOT event, and this test was done assuming that the measured bytes
						// would include the entire EFI_VARIABLE_DATA structure. Repeat the test with only the variable data.
						efiBootVariableBehaviourTry = efiBootVariableBehaviourVarDataOnly
						continue Loop
					}
					// Record the expected digest on the event
					expectedMeasuredBytes := out.expectedMeasuredBytes(false)
					h := alg.GetHash().New()
					h.Write(expectedMeasuredBytes)
					out.incorrectDigestValues = append(out.incorrectDigestValues, incorrectDigestValue{algorithm: alg, expected: h.Sum(nil)})

					out.measuredBytes = nil
					out.measuredTrailingBytes = nil

					return
				}
			}
		}
	}
	return
}

type logChecker struct {
	algs                     tcglog.AlgorithmIdList
	expectedPCRValues        map[tcglog.PCRIndex]tcglog.DigestMap
	efiBootVariableBehaviour efiBootVariableBehaviour
	events                   []*checkedEvent
}

func (c *logChecker) ensureExpectedPCRValuesInitialized(index tcglog.PCRIndex) {
	if _, exists := c.expectedPCRValues[index]; exists {
		return
	}

	c.expectedPCRValues[index] = tcglog.DigestMap{}

	for _, alg := range c.algs {
		c.expectedPCRValues[index][alg] = make(tcglog.Digest, alg.Size())
	}
}

func (c *logChecker) simulatePCRExtend(event *checkedEvent) {
	if !event.extendsPCR() {
		return
	}

	for alg, digest := range event.Digests {
		h := alg.GetHash().New()
		h.Write(c.expectedPCRValues[event.PCRIndex][alg])
		h.Write(digest)
		c.expectedPCRValues[event.PCRIndex][alg] = h.Sum(nil)
	}
}

func (c *logChecker) processEvent(event *tcglog.Event) {
	c.ensureExpectedPCRValuesInitialized(event.PCRIndex)

	ce := checkEvent(event, c)

	c.simulatePCRExtend(ce)
	c.events = append(c.events, ce)
}

func (c *logChecker) run(log *tcglog.Log) {
	c.algs = log.Algorithms
	c.expectedPCRValues = make(map[tcglog.PCRIndex]tcglog.DigestMap)

	for _, event := range log.Events {
		c.processEvent(event)
	}
}

func main() {
	flag.Parse()

	args := flag.Args()
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "Too many arguments\n")
		os.Exit(1)
	}

	if !noDefaultPcrs {
		pcrs = append(pcrs, 0, 1, 2, 3, 4, 5, 6, 7)
		if withGrub {
			pcrs = append(pcrs, 8, 9)
		}
	}

	sort.SliceStable(pcrs, func(i, j int) bool { return pcrs[i] < pcrs[j] })

	if logPath == "" {
		if filepath.Dir(tpmPath) != "/dev" {
			fmt.Fprintf(os.Stderr, "Expected TPM path to be a device node in /dev")
			os.Exit(1)
		}
		logPath = fmt.Sprintf("/sys/kernel/security/%s/binary_bios_measurements", filepath.Base(tpmPath))
	} else {
		tpmPath = ""
	}

	f, err := os.Open(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open log: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	log, err := tcglog.ParseLog(f, &tcglog.LogOptions{EnableGrub: withGrub, EnableSystemdEFIStub: withSdEfiStub, SystemdEFIStubPCR: tcglog.PCRIndex(sdEfiStubPcr)})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse log: %v\n", err)
		os.Exit(1)
	}

	c := &logChecker{}
	c.run(log)

	if c.efiBootVariableBehaviour == efiBootVariableBehaviourVarDataOnly {
		fmt.Printf("- EV_EFI_VARIABLE_BOOT events only contain measurement of variable data rather than the entire UEFI_VARIABLE_DATA structure\n\n")
	}

	seenInvalidData := false
	for _, e := range c.events {
		err := e.dataDecoderErr()
		if err == nil {
			continue
		}

		if !seenInvalidData {
			seenInvalidData = true
			fmt.Printf("- The following events contain event data that was not in the expected format and could not be decoded correctly:\n")
		}

		fmt.Printf("  - Event %d in PCR %d (type: %s): %v\n", e.Index, e.PCRIndex, e.EventType, err)
	}
	if seenInvalidData {
		fmt.Printf("\n\n")
	}

	seenTrailingMeasuredBytes := false
	for _, e := range c.events {
		if len(e.measuredTrailingBytes) == 0 {
			continue
		}

		if !seenTrailingMeasuredBytes {
			seenTrailingMeasuredBytes = true
			fmt.Printf("- The following events have trailing bytes at the end of their event data that was hashed and measured:\n")
		}

		fmt.Printf("  - Event %d in PCR %d (type: %s): %x (%d bytes)\n", e.Index, e.PCRIndex, e.EventType, e.measuredTrailingBytes, len(e.measuredTrailingBytes))
	}
	if seenTrailingMeasuredBytes {
		fmt.Printf("  This trailing bytes should be taken in to account when pre-computing digests for these events when the components " +
			"being measured are updated or changed in some way.\n\n")
	}

	seenIncorrectDigests := false
	for _, e := range c.events {
		if len(e.incorrectDigestValues) == 0 {
			continue
		}

		if !seenIncorrectDigests {
			seenIncorrectDigests = true
			fmt.Printf("- The following events have digests that aren't consistent with the data recorded with them in the log:\n")
		}

		for _, d := range e.incorrectDigestValues {
			fmt.Printf("  - Event %d in PCR %d (type: %s, alg: %s) - expected (from data): %x, got: %x\n", e.Index, e.PCRIndex, e.EventType, d.algorithm, d.expected, e.Digests[d.algorithm])
		}
	}
	if seenIncorrectDigests {
		fmt.Printf("  This is unexpected for these event types. Knowledge of the format of the data being measured is required in order " +
			"to pre-compute digests for these events when the components being measured are updated or changed in some way.\n\n")
	}

	if tpmPath == "" {
		fmt.Printf("- Expected PCR values from log:\n")
		for _, i := range pcrs {
			for _, alg := range log.Algorithms {
				fmt.Printf("PCR %d, bank %s: %x\n", i, alg, c.expectedPCRValues[i][alg])
			}
		}
		return
	}

	tpmPCRValues, err := readPCRs(log.Algorithms)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot read PCR values from TPM: %v", err)
		os.Exit(1)
	}

	seenLogConsistencyError := false
	for _, i := range pcrs {
		for _, alg := range log.Algorithms {
			if bytes.Equal(c.expectedPCRValues[i][alg], tpmPCRValues[i][alg]) {
				continue
			}
			if !seenLogConsistencyError {
				seenLogConsistencyError = true
				fmt.Printf("- The log is not consistent with what was measured in to the TPM for some PCRs:\n")
			}
			fmt.Printf("  - PCR %d, bank %s - actual value from TPM: %x, expected value from log: %x\n",
				i, alg, tpmPCRValues[i][alg], c.expectedPCRValues[i][alg])
		}
	}

	if seenLogConsistencyError {
		fmt.Printf("*** The event log is broken! ***\n")
	}
}
