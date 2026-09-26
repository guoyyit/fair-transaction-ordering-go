package fairorder

import "crypto/sha256"

// 承诺与排序键均使用 SHA-256：公开、确定性、不可逆（抗原像）。
//
// 承诺格式使用长度前缀编码盐值与原文，避免两段拼接产生歧义：
//
//	commitment = SHA256( u32(len(salt)) || salt || u32(len(payload)) || payload )
//
// 排序键为交易原文本身的哈希，与到达顺序、提交时间无关：
//
//	order_key  = SHA256( payload )
//
// 任何参与方都可独立复算这两个值进行校验。

func hashCommitment(salt, payload []byte) [32]byte {
	h := sha256.New()
	// 长度前缀，防止 salt/payload 边界歧义。
	var lb [4]byte
	lb[0], lb[1], lb[2], lb[3] = byte(len(salt)>>24), byte(len(salt)>>16), byte(len(salt)>>8), byte(len(salt))
	h.Write(lb[:])
	h.Write(salt)
	lb[0], lb[1], lb[2], lb[3] = byte(len(payload)>>24), byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload))
	h.Write(lb[:])
	h.Write(payload)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// MakeCommitment 根据揭示盐值与交易原文生成不可逆承诺。
func MakeCommitment(salt, payload []byte) []byte {
	c := hashCommitment(salt, payload)
	return c[:]
}

// VerifyCommitment 校验揭示内容与提交阶段承诺是否对应。
// 使用恒定长度比较，避免时序侧信道。
func VerifyCommitment(commitment, salt, payload []byte) bool {
	if len(commitment) != sha256.Size {
		return false
	}
	got := hashCommitment(salt, payload)
	var diff byte
	for i := 0; i < sha256.Size; i++ {
		diff |= commitment[i] ^ got[i]
	}
	return diff == 0
}

// OrderKey 返回公开、可独立复算的交易排序键 SHA256(payload)。
func OrderKey(payload []byte) []byte {
	k := sha256.Sum256(payload)
	return k[:]
}
