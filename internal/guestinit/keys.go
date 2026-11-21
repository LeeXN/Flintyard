package guestinit

import (
	"crypto/elliptic"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/big"
	"strings"
	"unicode"
	"unicode/utf8"
)

// PublicKeys accepts public-key records only, without authorized_keys options
// such as command=. Comments are discarded; private keys and malformed SSH wire
// payloads never reach the seed. No crypto/ssh dependency or authentication is
// needed for this data-only first-boot protocol.
func PublicKeys(data []byte) (string, error) {
	if len(data) == 0 || len(data) > 64<<10 || !utf8.Valid(data) {
		return "", fmt.Errorf("public keys must be nonempty UTF-8, at most 64 KiB")
	}
	if strings.ContainsFunc(string(data), func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) {
		return "", fmt.Errorf("control character in public keys")
	}
	var keys []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 || len(line) > 16<<10 || len(keys) >= 32 {
			return "", fmt.Errorf("invalid public key record or key limit")
		}
		wire, err := base64.StdEncoding.Strict().DecodeString(parts[1])
		if err != nil || !validKey(parts[0], wire) {
			return "", fmt.Errorf("unsupported or malformed public key (expected ed25519, RSA or NIST ECDSA)")
		}
		keys = append(keys, parts[0]+" "+base64.StdEncoding.EncodeToString(wire))
	}
	if len(keys) == 0 {
		return "", fmt.Errorf("public key file contains no keys")
	}
	return strings.Join(keys, "\n") + "\n", nil
}

func sshField(wire *[]byte) ([]byte, bool) {
	if len(*wire) < 4 {
		return nil, false
	}
	n := uint64(binary.BigEndian.Uint32((*wire)[:4]))
	if n > uint64(len(*wire)-4) {
		return nil, false
	}
	b := (*wire)[4 : 4+int(n)]
	*wire = (*wire)[4+int(n):]
	return b, true
}
func positiveMPInt(b []byte) bool {
	return len(b) > 0 && b[0]&0x80 == 0 && (b[0] != 0 || len(b) > 1 && b[1]&0x80 != 0)
}
func validKey(kind string, wire []byte) bool {
	typ, ok := sshField(&wire)
	if !ok || string(typ) != kind {
		return false
	}
	switch kind {
	case "ssh-ed25519":
		key, ok := sshField(&wire)
		return ok && len(key) == 32 && len(wire) == 0
	case "ssh-rsa":
		e, ok := sshField(&wire)
		if !ok || !positiveMPInt(e) || len(e) > 4 {
			return false
		}
		exponent := new(big.Int).SetBytes(e).Uint64()
		if exponent < 3 || exponent > 1<<31-1 || exponent%2 == 0 {
			return false
		}
		n, ok := sshField(&wire)
		if !ok || !positiveMPInt(n) || len(n) > 2049 || len(wire) != 0 {
			return false
		}
		modulus := new(big.Int).SetBytes(n)
		return modulus.BitLen() >= 2048 && modulus.BitLen() <= 16384 && modulus.Bit(0) == 1
	case "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521":
		name, ok := sshField(&wire)
		if !ok || "ecdsa-sha2-"+string(name) != kind {
			return false
		}
		point, ok := sshField(&wire)
		if !ok || len(wire) != 0 {
			return false
		}
		curve := map[string]elliptic.Curve{"nistp256": elliptic.P256(), "nistp384": elliptic.P384(), "nistp521": elliptic.P521()}[string(name)]
		x, _ := elliptic.Unmarshal(curve, point)
		return x != nil
	}
	return false
}
