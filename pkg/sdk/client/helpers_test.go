package client

import (
	"encoding/json"

	"github.com/luxfi/zap"
)

// encodeTestResponseFrame builds the ZAP envelope frame that the
// production zap.Node would return from a server handler. Kept in the
// test file so the production file carries no test-only code paths.
func encodeTestResponseFrame(opcode uint16, status uint32, detail string, body []byte) []byte {
	b := zap.NewBuilder(len(body) + envelopeObjectSize + 64)
	obj := b.StartObject(envelopeObjectSize)
	obj.SetBytes(envelopeBody, body)
	obj.SetUint32(envelopeStatus, status)
	if detail != "" {
		obj.SetBytes(envelopeError, []byte(detail))
	}
	obj.FinishAsRoot()
	return b.FinishWithFlags(uint16(opcode) << 8)
}

// decodeStubRequestBody decodes the JSON body that clientImpl.roundTrip
// hands to Transport.Call. Requests are delivered to Transport as raw
// JSON bytes (the envelope is applied inside the zapTransport Call
// implementation), so tests can inspect the payload without parsing a
// ZAP frame.
func decodeStubRequestBody(body []byte, out any) error {
	return json.Unmarshal(body, out)
}
