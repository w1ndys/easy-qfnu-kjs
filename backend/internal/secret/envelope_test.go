package secret

// 本文件的用例全部是纯函数：不碰文件系统、不碰网络，只验证信封格式与 GCM 的认证行为。
// 信封是「把业务秘密写进库」之前的最后一道形态，篡改与 AAD 错位都必须失败，
// 因此断言的重点不是错误文案，而是「解不开」这一结果；Open 的失败用例统一过 assertOpenRejected，
// 顺带钉住「失败不返回明文、错误被 ErrEnvelopeInvalid 包裹、文案不回显信封与明文」。

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// envelopeTestKey 返回一把确定性的测试密钥：seed 不同则密钥不同，便于构造「换密钥解不开」。
// 密钥内容与生产无关，用例内可预测反而方便复现。
func envelopeTestKey(seed byte) Key {
	var key Key
	for i := range key {
		key[i] = seed + byte(i)
	}
	return key
}

// mustSeal 用给定密钥封住明文，失败即终止用例。
func mustSeal(t *testing.T, key Key, aad string, plaintext []byte) string {
	t.Helper()
	envelope, err := key.Seal(aad, plaintext)
	if err != nil {
		t.Fatalf("Seal(%q) 失败: %v", aad, err)
	}
	return envelope
}

// envelopePayload 把信封拆回原始载荷，同时断言前缀与 base64url 形态：
// 这是所有篡改用例的起点，格式一旦走样，后面的断言就没有意义。
func envelopePayload(t *testing.T, envelope string) []byte {
	t.Helper()
	if !strings.HasPrefix(envelope, envelopePrefix) {
		t.Fatalf("信封 %q 缺少 %s 前缀", envelope, envelopePrefix)
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(envelope[len(envelopePrefix):])
	if err != nil {
		t.Fatalf("信封 %q 不是合法 base64url: %v", envelope, err)
	}
	return payload
}

// buildEnvelope 把原始载荷重新编码成信封，用来构造篡改样本。
func buildEnvelope(payload []byte) string {
	return envelopePrefix + base64.RawURLEncoding.EncodeToString(payload)
}

// assertOpenRejected 统一断言 Open 的失败形态，四条缺一不可：
// 错误被 ErrEnvelopeInvalid 包裹（上层可 errors.Is）、不返回明文、
// 错误文案不回显信封或信封载荷、也不含明文——失败路径最容易把密文抄进日志。
// plaintext 传空串表示该用例没有可比对的明文。
func assertOpenRejected(t *testing.T, envelope, plaintext string, got []byte, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("信封（%d 字节）不应解开，得到 %x", len(envelope), got)
	}
	if !errors.Is(err, ErrEnvelopeInvalid) {
		t.Fatalf("错误 %q 应包裹 ErrEnvelopeInvalid，供上层 errors.Is 判定", err)
	}
	if got != nil {
		t.Fatalf("失败时不应返回明文，实际 %x", got)
	}
	if envelope != "" && strings.Contains(err.Error(), envelope) {
		t.Fatalf("错误文案不应回显信封内容：%v", err)
	}
	if payload := strings.TrimPrefix(envelope, envelopePrefix); payload != "" &&
		strings.Contains(err.Error(), payload) {
		t.Fatalf("错误文案不应回显信封载荷：%v", err)
	}
	if plaintext != "" && strings.Contains(err.Error(), plaintext) {
		t.Fatalf("错误文案不应包含明文：%v", err)
	}
}

// TestSealOpenRoundTrip 覆盖往返一致性：空明文、单字节、1 KiB 二进制与多字节 UTF-8
// 在两组业务键名下都能原样解回，且信封长度恰为 nonce+密文+tag（GCM 不做填充）。
func TestSealOpenRoundTrip(t *testing.T) {
	big := make([]byte, 1024)
	for i := range big {
		big[i] = byte(i % 256)
	}

	cases := []struct {
		name      string
		plaintext []byte
	}{
		{name: "空明文", plaintext: []byte{}},
		{name: "单字节", plaintext: []byte{0x00}},
		{name: "1 KiB 二进制", plaintext: big},
		{name: "UTF-8 中文", plaintext: []byte("教务密码：含中文与标点，长度不固定")},
	}

	aads := []string{"settings/jw_password", "settings/feishu_app_secret"}
	key := envelopeTestKey(0)

	for _, tc := range cases {
		for _, aad := range aads {
			t.Run(tc.name+"/"+aad, func(t *testing.T) {
				envelope := mustSeal(t, key, aad, tc.plaintext)

				payload := envelopePayload(t, envelope)
				if want := nonceLen + len(tc.plaintext) + gcmTagLen; len(payload) != want {
					t.Fatalf("载荷长度 = %d，应为 %d", len(payload), want)
				}

				got, err := key.Open(aad, envelope)
				if err != nil {
					t.Fatalf("Open(%q) 失败: %v", aad, err)
				}
				if !bytes.Equal(got, tc.plaintext) {
					t.Fatalf("往返结果不一致：得到 %x，应为 %x", got, tc.plaintext)
				}
			})
		}
	}
}

// TestSealFormat 固定信封的外部形态：v1: 前缀、无填充、只用 URL 安全字符、
// 载荷以 12 字节 nonce 开头，且信封字符串与解码后的载荷里都不应出现明文（加密不是编码）。
func TestSealFormat(t *testing.T) {
	key := envelopeTestKey(1)
	// 明文用 ASCII：只有 ASCII 明文才可能以原样字节出现在信封字符串里，中文经 UTF-8
	// 编码后与 base64url 字符集天然不相交，拿它做「不含明文」的断言没有区分力。
	plaintext := []byte("this-plaintext-must-not-appear-in-the-envelope")

	envelope := mustSeal(t, key, "settings/jw_password", plaintext)

	if !strings.HasPrefix(envelope, "v1:") {
		t.Fatalf("信封应以 v1: 开头，实际 %q", envelope)
	}
	encoded := envelope[len(envelopePrefix):]
	if strings.ContainsRune(encoded, '=') {
		t.Fatalf("信封不应含 base64 填充符，实际 %q", encoded)
	}
	for _, r := range encoded {
		urlSafe := r == '-' || r == '_' ||
			('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9')
		if !urlSafe {
			t.Fatalf("信封含非 base64url 字符 %q：%q", r, encoded)
		}
	}
	if strings.Contains(envelope, string(plaintext)) {
		t.Fatal("信封里不应出现明文")
	}

	payload := envelopePayload(t, envelope)
	if len(payload) < nonceLen {
		t.Fatalf("载荷 %d 字节，不足 12 字节 nonce", len(payload))
	}
	if bytes.Contains(payload, plaintext) {
		t.Fatal("解码后的载荷里不应出现明文")
	}
}

// TestSealUsesFreshNonce 确认每次 Seal 都换 nonce：同一密钥、同一明文、同一 AAD
// 连续 64 次，nonce 互不相同，信封也互不相同（GCM 复用 nonce 会直接泄密，CP12）。
func TestSealUsesFreshNonce(t *testing.T) {
	key := envelopeTestKey(2)
	plaintext := []byte("nonce 每次都必须重新生成")
	const rounds = 64

	nonces := make(map[string]int, rounds)
	envelopes := make(map[string]int, rounds)
	for i := range rounds {
		envelope := mustSeal(t, key, "settings/jw_password", plaintext)

		nonce := string(envelopePayload(t, envelope)[:nonceLen])
		if prev, dup := nonces[nonce]; dup {
			t.Fatalf("第 %d 次 Seal 复用了第 %d 次的 nonce：%x", i, prev, nonce)
		}
		nonces[nonce] = i

		if prev, dup := envelopes[envelope]; dup {
			t.Fatalf("第 %d 次 Seal 与第 %d 次产生相同信封", i, prev)
		}
		envelopes[envelope] = i

		// 复用 nonce 之外还要确认每次都真的解得开：nonce 变了但格式错，单看去重也会过。
		got, err := key.Open("settings/jw_password", envelope)
		if err != nil || !bytes.Equal(got, plaintext) {
			t.Fatalf("第 %d 次 Seal 的结果解不回明文：err=%v", i, err)
		}
	}
}

// TestOpenRejectsByteTamper 逐字节篡改：nonce、密文、tag 的每一个字节各翻转一次，
// Open 都必须失败。翻转后重新按规范 base64url 编码，因此失败只能来自 GCM 校验，
// 而不是编码形态——这条用例证明的是认证标签确实覆盖了整段载荷。
func TestOpenRejectsByteTamper(t *testing.T) {
	key := envelopeTestKey(3)
	plaintext := []byte("tamper-me-0123456789abcdef") // 26 字节，让密文段足够长
	aad := "settings/jw_password"

	envelope := mustSeal(t, key, aad, plaintext)
	payload := envelopePayload(t, envelope)

	if len(payload) != nonceLen+len(plaintext)+gcmTagLen {
		t.Fatalf("前置条件不成立：载荷 %d 字节", len(payload))
	}

	for i := range payload {
		tampered := bytes.Clone(payload)
		tampered[i] ^= 0x01
		tamperedEnvelope := buildEnvelope(tampered)

		region := "ciphertext"
		switch {
		case i < nonceLen:
			region = "nonce"
		case i >= nonceLen+len(plaintext):
			region = "tag"
		}

		t.Run(fmt.Sprintf("%s_byte_%d", region, i), func(t *testing.T) {
			got, err := key.Open(aad, tamperedEnvelope)
			assertOpenRejected(t, tamperedEnvelope, string(plaintext), got, err)
		})
	}
}

// TestOpenRejectsWrongAAD 覆盖 AAD 绑定：同一把密钥下，A 项的密文换成 B 项的 AAD
// 必须解不开；差一个字符、空串、前后缀相近都算不匹配（CP12）。
func TestOpenRejectsWrongAAD(t *testing.T) {
	key := envelopeTestKey(4)
	plaintext := []byte("ciphertext-bound-to-settings-jw_password")
	envelope := mustSeal(t, key, "settings/jw_password", plaintext)

	cases := []struct {
		name string
		aad  string
	}{
		{name: "换个设置项", aad: "settings/feishu_app_secret"},
		{name: "只差一个字符", aad: "settings/jw_password2"},
		{name: "去掉前缀", aad: "jw_password"},
		{name: "空 AAD", aad: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := key.Open(tc.aad, envelope)
			assertOpenRejected(t, envelope, string(plaintext), got, err)
		})
	}

	// 反向确认：AAD 不同则密文不同，各自只对自己的 AAD 有效。
	other := mustSeal(t, key, "settings/feishu_app_secret", plaintext)
	if other == envelope {
		t.Fatal("AAD 不同的两次 Seal 不应产生相同信封")
	}
	if _, err := key.Open("settings/feishu_app_secret", other); err != nil {
		t.Fatalf("B 项密文用 B 项 AAD 应能解开: %v", err)
	}
	got, err := key.Open("settings/jw_password", other)
	assertOpenRejected(t, other, string(plaintext), got, err)
}

// TestSealOpenRejectEmptyAAD 覆盖空 AAD：封与解都拒绝。空 AAD 等于放弃「密文绑定到
// 设置项」这层保护，而且只在解封侧拒绝会让 Seal 产出必然解不开的信封，所以两边都在入口拦。
func TestSealOpenRejectEmptyAAD(t *testing.T) {
	key := envelopeTestKey(10)
	plaintext := []byte("aad-must-not-be-empty")

	envelope, err := key.Seal("", plaintext)
	if err == nil {
		t.Fatalf("空 AAD 不应封出信封：%q", envelope)
	}
	if !errors.Is(err, ErrEnvelopeInvalid) {
		t.Fatalf("Seal 的错误 %q 应包裹 ErrEnvelopeInvalid", err)
	}
	if strings.Contains(err.Error(), string(plaintext)) {
		t.Fatalf("错误文案不应包含明文：%v", err)
	}

	// 用正常 AAD 封出合法信封，再拿空 AAD 去解：必须在动密钥之前就拒掉，
	// 而不是拖到 GCM 才失败，所以文案里要有 AAD。
	valid := mustSeal(t, key, "settings/jw_password", plaintext)
	got, err := key.Open("", valid)
	assertOpenRejected(t, valid, string(plaintext), got, err)
	if !strings.Contains(err.Error(), "AAD") {
		t.Fatalf("错误 %q 应指出 AAD 为空，确认走的是参数校验分支", err)
	}
}

// assertZeroKeyError 统一断言「零值密钥」的错误形态：文案指出零值，且同时被
// model.ErrKeyUnavailable（上层据此认出「密钥没装好」而不是「这份密文坏了」）与
// ErrEnvelopeInvalid（与其它信封失败同一分类）包裹。Seal 与 Open 共用 aead() 入口，
// 两条路径的 errors.Is 结果必须完全一致，否则上层在封与解两侧会拿到不同分类。
func assertZeroKeyError(t *testing.T, op string, err error) {
	t.Helper()
	if !strings.Contains(err.Error(), "零值") {
		t.Fatalf("%s 的错误 %q 应指出密钥为零值", op, err)
	}
	if !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("%s 的错误 %q 应包裹 model.ErrKeyUnavailable", op, err)
	}
	if !errors.Is(err, ErrEnvelopeInvalid) {
		t.Fatalf("%s 的错误 %q 应包裹 ErrEnvelopeInvalid", op, err)
	}
}

// TestSealOpenRejectZeroKey 覆盖零值密钥：32 个零字节也是合法 AES 密钥，
// aes.NewCipher 不会报错，必须在 aead() 入口拒绝，否则等于用一把人尽皆知的密钥加密；
// 拒绝时同时包裹 ErrKeyUnavailable 与 ErrEnvelopeInvalid，且 Seal 与 Open 一致。
func TestSealOpenRejectZeroKey(t *testing.T) {
	var zero Key
	plaintext := []byte("zero-key-is-not-a-key")

	envelope, err := zero.Seal("settings/jw_password", plaintext)
	if err == nil {
		t.Fatalf("零值密钥不应封出信封：%q", envelope)
	}
	assertZeroKeyError(t, "Seal", err)

	// 格式合法的信封 + 零值密钥：Open 的其它校验都能过，必须停在密钥这一步。
	valid := mustSeal(t, envelopeTestKey(11), "settings/jw_password", plaintext)
	got, openErr := zero.Open("settings/jw_password", valid)
	assertOpenRejected(t, valid, string(plaintext), got, openErr)
	assertZeroKeyError(t, "Open", openErr)
}

// TestOpenRejectsMissingOrUnknownPrefix 覆盖两种前缀错误：完全没有 v1: 前缀（空串、裸载荷、
// 缺冒号）报「缺少」，有冒号但版本不是 v1（v2:、V1:、别的算法）报「不支持」；
// v1: 后为空则前缀通过、落到载荷长度门。
func TestOpenRejectsMissingOrUnknownPrefix(t *testing.T) {
	key := envelopeTestKey(5)
	payload := envelopePayload(t, mustSeal(t, key, "settings/jw_password", []byte("x")))
	encoded := base64.RawURLEncoding.EncodeToString(payload)

	cases := []struct {
		name     string
		envelope string
		wantWord string // 错误文案里应出现的关键词，用来确认走的是预期的分支
	}{
		{name: "空信封", envelope: "", wantWord: "缺少"},
		{name: "缺少前缀的裸载荷", envelope: encoded, wantWord: "缺少"},
		{name: "未知版本 v2", envelope: "v2:" + encoded, wantWord: "版本"},
		{name: "大小写变体 V1", envelope: "V1:" + encoded, wantWord: "版本"},
		{name: "缺冒号", envelope: "v1" + encoded, wantWord: "缺少"},
		{name: "v1 后为空", envelope: "v1:", wantWord: "不足"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := key.Open("settings/jw_password", tc.envelope)
			assertOpenRejected(t, tc.envelope, "", got, err)
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Fatalf("错误 %q 应包含 %q，确认走的是预期分支", err, tc.wantWord)
			}
		})
	}
}

// TestOpenRejectsInvalidBase64 覆盖编码层：非 base64url 字符、填充符、标准 base64 的
// +/、以及非零尾比特都必须被 Strict 拒掉，否则同一载荷会有多种写法。
func TestOpenRejectsInvalidBase64(t *testing.T) {
	key := envelopeTestKey(6)

	// 非零尾比特样本：28 字节全零的 base64url 是 38 个 'A'，只把末字符换成 'B'，
	// 动的正是末尾 4 个填充位。非 Strict 解码会忽略它们，照样得到 28 字节并通过长度门，
	// 直到 GCM 才失败；只有 Strict 生效才会在编码层就拒绝。用短载荷（如以前的 "AAB"）
	// 分不出这两种情况——它本来就会落到长度门，Strict 是否生效对结果没有影响。
	nonZeroTail := strings.Repeat("A", 37) + "B"
	decoded, err := base64.RawURLEncoding.DecodeString(nonZeroTail)
	if err != nil || len(decoded) != nonceLen+gcmTagLen {
		t.Fatalf("前置条件不成立：非 Strict 解码得到 %d 字节，err=%v", len(decoded), err)
	}

	cases := []struct {
		name    string
		payload string
	}{
		{name: "非法字符", payload: "!!!!"},
		{name: "含填充符", payload: "QUJDRA=="},
		{name: "标准 base64 字符 +", payload: "a+b/"},
		{name: "非零尾比特", payload: nonZeroTail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envelope := envelopePrefix + tc.payload
			got, err := key.Open("settings/jw_password", envelope)
			assertOpenRejected(t, envelope, "", got, err)
			// 失败必须来自编码层而不是 GCM：若 Strict 失效，非零尾比特样本会被解成
			// 28 字节全零、通过长度门，最终报「解密信封」。
			if !strings.Contains(err.Error(), "base64url") {
				t.Fatalf("错误 %q 应在编码层拒绝，确认走的是 Strict 解码", err)
			}
		})
	}
}

// TestOpenRejectsShortEnvelope 覆盖长度门：载荷短于 nonce(12)+tag(16) 时连切分都不成立，
// 必须在初始化 AES-GCM 之前按格式拒绝；恰好达到下界（28 字节全零）则应落到 tag 校验失败。
func TestOpenRejectsShortEnvelope(t *testing.T) {
	key := envelopeTestKey(7)
	const minimum = nonceLen + gcmTagLen

	for size := range minimum {
		payload := bytes.Repeat([]byte{0x5a}, size)
		envelope := buildEnvelope(payload)
		got, err := key.Open("settings/jw_password", envelope)
		assertOpenRejected(t, envelope, "", got, err)
		if !strings.Contains(err.Error(), "不足") {
			t.Fatalf("载荷 %d 字节短于下界 %d，错误 %q 应指出长度不足", size, minimum, err)
		}
	}

	// 边界：正好 28 字节，长度门放行，失败来自 GCM tag。
	envelope := buildEnvelope(make([]byte, minimum))
	got, err := key.Open("settings/jw_password", envelope)
	assertOpenRejected(t, envelope, "", got, err)
	if !strings.Contains(err.Error(), "解密信封") {
		t.Fatalf("全零的 %d 字节载荷应落到 GCM 校验，错误 %q 不符合预期", minimum, err)
	}
}

// TestOpenRejectsOversizedEnvelope 覆盖 64 KiB 长度门：长度判定排在 base64 解码之前，
// 超长信封一个字节的内容都不解码；恰好等于上限则放行到解码层。
//
// 两个样本都用非法 base64url 字符 "!" 填充，这样「有没有走到解码层」有唯一可观测的证据：
// 超长样本的错误里必须出现「超过上限」且不得出现「base64url」（出现就说明先去解码了）；
// 边界样本的错误里必须出现「base64url」，证明它确实越过了长度门、落在 Strict 解码这一步。
// 若改用全 'A' 这类合法样本，超长时它也会在解码层失败，「长度门排在前面」就无从区分。
func TestOpenRejectsOversizedEnvelope(t *testing.T) {
	key := envelopeTestKey(12)
	plaintext := []byte("oversized-envelope")
	const filler = "!"

	overLimit := envelopePrefix + strings.Repeat(filler, maxEnvelopeLen-len(envelopePrefix)+1)
	if len(overLimit) != maxEnvelopeLen+1 {
		t.Fatalf("前置条件不成立：构造出 %d 字节", len(overLimit))
	}
	got, err := key.Open("settings/jw_password", overLimit)
	assertOpenRejected(t, overLimit, string(plaintext), got, err)
	if !strings.Contains(err.Error(), "超过上限") {
		t.Fatalf("错误 %q 应指出超过长度上限，确认长度门在解码之前", err)
	}
	if strings.Contains(err.Error(), "base64url") {
		t.Fatalf("错误 %q 不应提到 base64url：超长信封必须在解码层之前就被拒", err)
	}

	// 边界：恰好 maxEnvelopeLen 字节不触发长度门；内容仍全是非法字符，由 base64 层拒绝。
	atLimit := envelopePrefix + strings.Repeat(filler, maxEnvelopeLen-len(envelopePrefix))
	if len(atLimit) != maxEnvelopeLen {
		t.Fatalf("前置条件不成立：构造出 %d 字节", len(atLimit))
	}
	got, err = key.Open("settings/jw_password", atLimit)
	assertOpenRejected(t, atLimit, string(plaintext), got, err)
	if strings.Contains(err.Error(), "超过上限") {
		t.Fatalf("恰好 %d 字节不应触发长度门，错误 %q", maxEnvelopeLen, err)
	}
	if !strings.Contains(err.Error(), "base64url") {
		t.Fatalf("错误 %q 应来自 base64url 解码层，证明恰好 %d 字节越过了长度门", err, maxEnvelopeLen)
	}
}

// TestOpenRejectsOtherKey 覆盖换密钥：信封只对封它的那把密钥有效，拿另一把密钥
// 即便格式完全正确也必须失败（tag 与密钥相关）。
func TestOpenRejectsOtherKey(t *testing.T) {
	sealer := envelopeTestKey(8)
	other := envelopeTestKey(9)
	if sealer == other {
		t.Fatal("测试密钥不应相同")
	}

	plaintext := []byte("envelope-only-opens-with-its-own-key")
	envelope := mustSeal(t, sealer, "settings/jw_password", plaintext)

	got, err := other.Open("settings/jw_password", envelope)
	assertOpenRejected(t, envelope, string(plaintext), got, err)
	if got, err := sealer.Open("settings/jw_password", envelope); err != nil || string(got) != string(plaintext) {
		t.Fatalf("原密钥应能解开自己的信封：err=%v", err)
	}
}
