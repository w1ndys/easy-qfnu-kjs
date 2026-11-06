package secret

// 本文件是密钥的使用侧：把明文封进带版本前缀的密文信封，以及把信封解回明文。
// 信封格式固定为 "v1:" + base64url(nonce(12) || ciphertext || tag(16))，
// AAD 传业务键名（约定 settings/<key>），从而把密文绑定到具体设置项：把 A 项的密文
// 挪到 B 项时 AAD 不符，GCM 校验直接失败（design.md「密文信封格式」、CP12）。
//
// 选 AES-256-GCM 而不是 XChaCha20-Poly1305 是 D3 的结论：标准库自带、不引入新依赖、
// amd64/arm64 都有 AES 加速；本系统加密次数以「管理员改设置」计，12 字节随机 nonce
// 的碰撞概率可忽略。前缀留给将来换算法：v2 可以并存，旧密文仍按 v1 解开。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

const (
	// envelopePrefix 是当前唯一支持的版本前缀，冒号是格式的一部分。
	envelopePrefix = "v1:"

	// nonceLen 是 GCM 的 nonce 字节数：每条密文都重新随机生成，绝不复用。
	nonceLen = 12

	// gcmTagLen 是 GCM 认证标签的字节数，也是 Open 判定「长度不足」的下界之一。
	gcmTagLen = 16

	// maxEnvelopeLen 是 Open 接受的整个信封（含 v1: 前缀）的字节上限。
	// 正常设置项只有几十字节，64 KiB 已经宽到不可能误伤，却能挡住「库里的脏数据
	// 被整段读进内存再解码」；长度门排在解码之前，超长信封连 base64 都不碰。
	maxEnvelopeLen = 64 << 10
)

// ErrEnvelopeInvalid 是信封不可用的哨兵：AAD 为空、信封超长、版本前缀缺失或不认识、
// 编码非法、载荷过短、GCM 认证失败都用 %w 包裹它。上层（pipeline/api）只要
// errors.Is(err, ErrEnvelopeInvalid) 就能判定「这份密文不可用」，不必匹配错误文案。
var ErrEnvelopeInvalid = errors.New("信封无效")

// Seal 用当前密钥把 plaintext 封成 "v1:" + base64url(nonce||ciphertext||tag)，
// AAD 传入业务键名（如 "settings/jw_password"），解封时必须传同一串。
//
// 每次调用都向 crypto/rand 取 12 字节新 nonce：GCM 的安全性要求「同一密钥下 nonce 不重复」，
// 这里不缓存也不递增，随机值在「加密次数以管理员改设置计」的量级下碰撞概率可忽略（D3）。
// AAD 为空时直接拒绝：空 AAD 等于放弃「密文绑定到设置项」这层保护，而且解封侧同样拒绝空 AAD，
// 封出来的信封必然解不开，不如在入口就失败。
// 返回的错误分两类：AAD 为空、零值密钥与 GCM 初始化失败都包裹 ErrEnvelopeInvalid，
// 其中零值密钥还同时包裹 model.ErrKeyUnavailable，与 Open 的 errors.Is 结果保持一致；
// 随机源失败则只带底层原因——那是系统故障，不是「这份密文不可用」。
// 后两种初始化失败对 32 字节密钥实际不可达，保留它是为了不静默吞掉标准库的签名变化。
func (k Key) Seal(aad string, plaintext []byte) (string, error) {
	if aad == "" {
		return "", fmt.Errorf("%w: AAD 不能为空", ErrEnvelopeInvalid)
	}

	aead, err := k.aead()
	if err != nil {
		return "", err
	}

	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("生成信封 nonce: %w", err)
	}

	// dst 先放 nonce，再把 ciphertext||tag 追加在后面：一次分配就得到完整的信封载荷，
	// 不必先 Seal 再拼接拷贝。gcm.Seal 会复用 dst 的容量，返回的切片以 nonce 开头。
	payload := make([]byte, nonceLen, nonceLen+len(plaintext)+gcmTagLen)
	copy(payload, nonce)
	payload = aead.Seal(payload, nonce, plaintext, []byte(aad))

	return envelopePrefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

// Open 校验信封的长度、版本前缀、base64url、载荷长度与 GCM tag，全部通过才返回明文；
// AAD 为空、信封超过 64 KiB、前缀缺失或版本未知、编码非法、载荷短于 nonce(12)+tag(16)、
// AAD 不符、密文或 tag 被改动，任一项都返回错误且不返回任何明文。
//
// 校验顺序是「先便宜后昂贵」：AAD 与字符串长度只处理参数，版本、base64url 与载荷长度
// 都只处理字符串，不需要动密钥；只有格式过关才初始化 AES-GCM。AAD 不单独比对——它作为
// GCM 的附加认证数据参与 tag 计算，AAD 不符与密文篡改在密码学上是同一类失败，都表现为
// tag 校验不过。所有失败都用 %w 包裹 ErrEnvelopeInvalid，上层可 errors.Is 判定；
// 错误里不出现信封内容与明文，避免调用方把密文或旧密钥材料抄进日志。
func (k Key) Open(aad, envelope string) ([]byte, error) {
	if aad == "" {
		return nil, fmt.Errorf("%w: AAD 不能为空", ErrEnvelopeInvalid)
	}

	// 长度门排在一切解码之前：超长信封连 base64 都不做，错误里只报字节数，不回显内容。
	if len(envelope) > maxEnvelopeLen {
		return nil, fmt.Errorf("%w: 信封 %d 字节，超过上限 %d 字节",
			ErrEnvelopeInvalid, len(envelope), maxEnvelopeLen)
	}

	// base64url 的字母表里没有冒号，所以「找不到冒号」等价于「版本前缀整个缺失」；
	// 找到冒号但版本不是 v1 则是另一类错误（v2:、V1: 或别的算法前缀），分开报，
	// 让调用方一眼看出是格式坏掉还是版本不兼容。
	version, body, ok := strings.Cut(envelope, ":")
	if !ok {
		return nil, fmt.Errorf("%w: 缺少 %s 前缀", ErrEnvelopeInvalid, envelopePrefix)
	}
	if version+":" != envelopePrefix {
		return nil, fmt.Errorf("%w: 不支持的版本前缀，只支持 %s", ErrEnvelopeInvalid, envelopePrefix)
	}

	payload, err := base64.RawURLEncoding.Strict().DecodeString(body)
	if err != nil {
		// 用 Strict 是刻意的：非零尾比特、填充符与标准 base64 的 +/ 都拒掉，
		// 保证同一份载荷只有一种合法写法，信封不会被改写成等价形态后仍被接受。
		return nil, fmt.Errorf("%w: 不是合法的 base64url: %w", ErrEnvelopeInvalid, err)
	}

	// 下界是 nonce+tag；空明文也正好是这个长度。比它短的信封连切分都无从谈起，
	// 直接按格式错误拒掉，不让 GCM 去猜。
	if len(payload) < nonceLen+gcmTagLen {
		return nil, fmt.Errorf("%w: 载荷 %d 字节，不足 nonce(%d) 与 tag(%d) 之和",
			ErrEnvelopeInvalid, len(payload), nonceLen, gcmTagLen)
	}

	aead, err := k.aead()
	if err != nil {
		// aead() 的错误已经带好分类（零值密钥同时包裹两个哨兵，其余分支也包裹 ErrEnvelopeInvalid），
		// 这里原样返回：再包一层会让文案出现两遍「信封无效」，Seal 与 Open 的分类也会不一致。
		return nil, err
	}

	plaintext, err := aead.Open(nil, payload[:nonceLen], payload[nonceLen:], []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("%w: 解密信封: %w", ErrEnvelopeInvalid, err)
	}
	return plaintext, nil
}

// aead 按当前密钥构造 AES-256-GCM 实例。
//
// 每次调用都新建 cipher 与 AEAD：cipher.AEAD 本身并发安全，不存在「共享就要加锁」的问题；
// 新建是为了不在值类型 Key 上挂状态——Key 会被复制、按值传递，缓存 AEAD 就得连带管理它的
// 生命周期，收益却只是省掉 AES 的密钥扩展（十轮子密钥展开，相对一次加解密可忽略）。
// 零值 Key 先挡掉：32 个零字节也是合法的 AES 密钥，aes.NewCipher 不会报错，放过去等于用
// 一把人尽皆知的密钥加密，因此必须在入口明确拒绝。零值密钥的错误同时包裹
// model.ErrKeyUnavailable 与 ErrEnvelopeInvalid：前者让上层认出「密钥没装好」，后者让信封
// 失败保持同一分类；aead() 是 Seal 与 Open 的共同入口，两边的 errors.Is 结果因此必然一致。
// 本函数的所有失败都包裹 ErrEnvelopeInvalid，Open 因而可以直接透传这里的错误，不再重复包裹。
// 末尾两段断言把「信封格式常量」与标准库的实际参数钉在一起：aes.NewCipher 只接受
// 16/24/32 字节，Key 恒为 32 字节所以这里实际不可达；cipher.NewGCM 的 nonce 与 tag
// 尺寸若将来被改，Seal 写出的格式会与 Open 的切分不一致，宁可在这里显式失败。
func (k Key) aead() (cipher.AEAD, error) {
	if k == (Key{}) {
		return nil, fmt.Errorf("%w: %w: 密钥为零值，无法构造 AES-256",
			model.ErrKeyUnavailable, ErrEnvelopeInvalid)
	}

	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, fmt.Errorf("%w: 初始化 AES-256: %w", ErrEnvelopeInvalid, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: 初始化 GCM: %w", ErrEnvelopeInvalid, err)
	}
	if size := aead.NonceSize(); size != nonceLen {
		return nil, fmt.Errorf("%w: GCM nonce 为 %d 字节，信封格式要求 %d 字节",
			ErrEnvelopeInvalid, size, nonceLen)
	}
	if size := aead.Overhead(); size != gcmTagLen {
		return nil, fmt.Errorf("%w: GCM tag 为 %d 字节，信封格式要求 %d 字节",
			ErrEnvelopeInvalid, size, gcmTagLen)
	}
	return aead, nil
}
