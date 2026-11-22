package browser

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1" //nolint:gosec
	"path/filepath"
	"testing"
)

// oldFormatCookie 造一条"没有域绑定"的 v10 Cookie：v10 ‖ AES-128-CBC(IV=16 空格, PKCS7, plain)，
// plain 就是 Cookie 值本身（老版本 Chrome 的格式），不带 SHA256(host_key) 前缀。
func oldFormatCookie(t *testing.T, plain []byte, password string, iter int) []byte {
	t.Helper()
	key, err := pbkdf2.Key(sha1.New, password, []byte("saltysalt"), iter, 16)
	if err != nil {
		t.Fatal(err)
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte("                ")).CryptBlocks(ct, padded)
	return append([]byte("v10"), ct...)
}

func classifyCookies(t *testing.T, cookies []testCookie) *ProfileClassification {
	t.Helper()
	dir := existingChromeProfile(t)
	writeCookiesDB(t, filepath.Join(dir, "Default", "Cookies"), cookies)
	cls, err := ClassifyProfile(dir)
	if err != nil {
		t.Fatal(err)
	}
	return cls
}

// D4：严格档（域绑定）失败时，采样的每一条都能用模拟口令解开（PKCS7 合法）且明文是合法 UTF-8 → mock。
func TestClassifyLenientTierAcceptsOldFormatMockCookies(t *testing.T) {
	for _, tc := range []struct {
		name     string
		password string
		iter     int
	}{{"macos", "mock_password", 1003}, {"linux basic", "peanuts", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			cls := classifyCookies(t, []testCookie{
				{".a.example", oldFormatCookie(t, []byte("session-token-0123456789"), tc.password, tc.iter)},
				{".b.example", oldFormatCookie(t, []byte("uid=42"), tc.password, tc.iter)},
				{".c.example", oldFormatCookie(t, []byte("中文值也是合法 UTF-8"), tc.password, tc.iter)},
			})
			if cls.OSCrypt != ProfileOSCryptMock || cls.Vault != ProfileVaultOpen || cls.Basis != ClassifyBasisMockLenient || cls.CookiesProbed != 3 {
				t.Fatalf("%+v", cls)
			}
		})
	}
}

// 严格档能认出来的，依据仍写严格档（宽松档只是兜底，不改变既有结论）。
func TestClassifyStrictTierStillWinsAndKeepsItsBasis(t *testing.T) {
	cls := classifyCookies(t, []testCookie{
		{".example.com", mockEncryptCookie(t, ".example.com", "token", "mock_password", 1003)},
	})
	if cls.Basis != ClassifyBasisMockDecrypts {
		t.Fatalf("%+v", cls)
	}
}

// 只要有一条不满足，就判 system：登录态丢失的后果对称，证据多的一方胜出——这里证据不全。
func TestClassifyLenientTierRequiresEverySampledCookie(t *testing.T) {
	good := oldFormatCookie(t, []byte("fine"), "mock_password", 1003)
	for name, bad := range map[string][]byte{
		"wrong key (padding/UTF-8 fails)": oldFormatCookie(t, []byte("encrypted under another password"), "keychain-password", 1003),
		"not UTF-8":                       oldFormatCookie(t, []byte{0xff, 0xfe, 0xfd, 0x80, 0x81}, "mock_password", 1003),
		"v11 (linux keyring)":             append([]byte("v11"), bytes.Repeat([]byte{7}, 32)...),
		"not block aligned":               append([]byte("v10"), bytes.Repeat([]byte{7}, 20)...),
	} {
		t.Run(name, func(t *testing.T) {
			cls := classifyCookies(t, []testCookie{{".ok.example", good}, {".bad.example", bad}})
			if cls.OSCrypt != ProfileOSCryptSystem || cls.Vault != ProfileVaultNone || cls.Basis != ClassifyBasisSystemCookies {
				t.Fatalf("one bad sample must keep the profile on the system keychain: %+v", cls)
			}
		})
	}
}

// 采样上限 20：前 20 条都满足就够了，后面的不看（探测成本有上限）。
func TestClassifyLenientTierSamplesAtMostTwenty(t *testing.T) {
	var cookies []testCookie
	for i := 0; i < lenientSampleLimit; i++ {
		cookies = append(cookies, testCookie{".ok.example", oldFormatCookie(t, []byte("value"), "mock_password", 1003)})
	}
	cookies = append(cookies, testCookie{".bad.example", oldFormatCookie(t, []byte{0xff, 0xfe}, "mock_password", 1003)})
	if cls := classifyCookies(t, cookies); cls.Basis != ClassifyBasisMockLenient {
		t.Fatalf("the 21st cookie is outside the sample: %+v", cls)
	}
	// 第 20 条坏 → 在样本里 → system。
	cookies[lenientSampleLimit-1] = testCookie{".bad.example", oldFormatCookie(t, []byte{0xff, 0xfe}, "mock_password", 1003)}
	if cls := classifyCookies(t, cookies); cls.OSCrypt != ProfileOSCryptSystem {
		t.Fatalf("the 20th cookie is inside the sample: %+v", cls)
	}
}

func TestMockCookiesDecryptLenientRejectsEmptySample(t *testing.T) {
	if mockCookiesDecryptLenient(nil) {
		t.Fatal("no evidence is not evidence")
	}
}
