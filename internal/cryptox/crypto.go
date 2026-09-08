package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

func DeriveKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" { return nil, fmt.Errorf("empty key") }
	if b, err := base64.RawStdEncoding.DecodeString(raw); err == nil && len(b)==32 { return b,nil }
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil && len(b)==32 { return b,nil }
	if b, err := hex.DecodeString(raw); err == nil && len(b)==32 { return b,nil }
	if len(raw) < 24 { return nil, fmt.Errorf("APP_ENCRYPTION_KEY must be at least 24 characters or a 32-byte base64/hex key") }
	h := sha256.Sum256([]byte(raw)); return h[:], nil
}

func Encrypt(key, plaintext []byte) (string,error) {
	block,err:=aes.NewCipher(key); if err!=nil{return "",err}
	gcm,err:=cipher.NewGCM(block); if err!=nil{return "",err}
	nonce:=make([]byte,gcm.NonceSize()); if _,err=io.ReadFull(rand.Reader,nonce);err!=nil{return "",err}
	ct:=gcm.Seal(nil,nonce,plaintext,nil)
	out:=append(nonce,ct...)
	return base64.RawURLEncoding.EncodeToString(out),nil
}
func Decrypt(key []byte, encoded string)([]byte,error){
	raw,err:=base64.RawURLEncoding.DecodeString(encoded);if err!=nil{return nil,err}
	block,err:=aes.NewCipher(key);if err!=nil{return nil,err};gcm,err:=cipher.NewGCM(block);if err!=nil{return nil,err}
	if len(raw)<gcm.NonceSize(){return nil,fmt.Errorf("ciphertext too short")}
	return gcm.Open(nil,raw[:gcm.NonceSize()],raw[gcm.NonceSize():],nil)
}
