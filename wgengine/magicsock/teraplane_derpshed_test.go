// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package magicsock

import (
	"encoding/binary"
	"testing"

	"github.com/tailscale/wireguard-go/device"
)

// The DERP bulk shed in sendAddr drops TRANSPORT (data) messages before
// bytes.Clone when a region's write channel is full, to stop a GC storm from
// wedging the send loops. Everything arriving via the wireguard-go bind has
// isDisco==false, INCLUDING WireGuard handshake traffic, so the shed must not
// drop it: a handshake/cookie shed under overload is exactly the recovery
// traffic a lapsed-trust peer needs, and dropping it risks pushing a DERP-only
// peer to RejectAfterTime. The type check is what protects it. This must key on
// TYPE, not size: bulk data is often small (a 20B inner packet is an ~88B
// datagram), so a size threshold would exempt exactly the small-but-high-rate
// data the shed exists to drop -- a measured -20% regression when it did.
func TestDerpShedNeverDropsWireGuardHandshakes(t *testing.T) {
	msg := func(typ uint32, n int) []byte {
		b := make([]byte, n)
		binary.LittleEndian.PutUint32(b[:4], typ)
		return b
	}
	// Handshake/cookie messages must NOT be classified as transport (never shed).
	for _, c := range []struct {
		name string
		typ  uint32
		size int
	}{
		{"handshake-initiation", device.MessageInitiationType, device.MessageInitiationSize},
		{"handshake-response", device.MessageResponseType, device.MessageResponseSize},
		{"cookie-reply", device.MessageCookieReplyType, device.MessageCookieReplySize},
	} {
		if isWireGuardTransport(msg(c.typ, c.size)) {
			t.Errorf("%s (type %d) classified as transport: the shed would drop it under overload", c.name, c.typ)
		}
	}
	// A small transport (data) datagram MUST be sheddable -- this is the frame
	// the size-based guard wrongly exempted.
	small := msg(device.MessageTransportType, device.MessageEncapsulatingTransportSize+device.MessageTransportHeaderSize+20)
	if !isWireGuardTransport(small) {
		t.Errorf("an %d-byte data datagram is not classified as transport: bulk would not be shed", len(small))
	}
	// A full-size transport datagram is also sheddable.
	big := msg(device.MessageTransportType, device.MessageEncapsulatingTransportSize+device.MessageTransportHeaderSize+1280)
	if !isWireGuardTransport(big) {
		t.Errorf("a %d-byte data datagram is not classified as transport", len(big))
	}
	// Degenerate short buffer: not transport (fail safe, never shed).
	if isWireGuardTransport([]byte{4, 0}) {
		t.Error("a truncated datagram must not be classified as transport")
	}
}
