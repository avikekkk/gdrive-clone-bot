package bot

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"regexp"
	"unicode/utf8"
)

// idTokenKey encrypts Drive IDs shown in search results so they are not
// directly readable, while staying copy-pasteable into /c and /n.
var idTokenKey = []byte{
	0x24, 0xeb, 0xb1, 0x72, 0x14, 0xf2, 0xfe, 0xa6,
	0x34, 0x0a, 0xc3, 0xb7, 0x14, 0xb7, 0xe2, 0xbf,
	0xa8, 0x58, 0xec, 0x5c, 0x77, 0xa2, 0xab, 0xdb,
	0x7d, 0xe2, 0x44, 0x96, 0xc9, 0xe7, 0x2f, 0x73,
}

var rawDriveIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{10,}$`)

// idTokenIV matches pyaes' AESModeOfOperationCTR default counter, which starts
// at 1, so tokens stay compatible with the ones the Python bot issued.
func idTokenIV() []byte {
	iv := make([]byte, aes.BlockSize)
	iv[aes.BlockSize-1] = 1
	return iv
}

func idTokenStream() (cipher.Stream, error) {
	block, err := aes.NewCipher(idTokenKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewCTR(block, idTokenIV()), nil
}

// encodeDriveIDToken encrypts a Drive ID into a base64 token.
func encodeDriveIDToken(fileID string) string {
	stream, err := idTokenStream()
	if err != nil {
		return ""
	}
	ciphertext := make([]byte, len(fileID))
	stream.XORKeyStream(ciphertext, []byte(fileID))
	return base64.StdEncoding.EncodeToString(ciphertext)
}

// decodeDriveIDToken returns the Drive ID behind a token, or "" if the input is
// not one of our tokens.
func decodeDriveIDToken(token string) string {
	ciphertext, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return ""
	}
	stream, err := idTokenStream()
	if err != nil {
		return ""
	}
	plaintext := make([]byte, len(ciphertext))
	stream.XORKeyStream(plaintext, ciphertext)

	decoded := string(plaintext)
	if !utf8.ValidString(decoded) || !rawDriveIDRe.MatchString(decoded) {
		return ""
	}
	return decoded
}
