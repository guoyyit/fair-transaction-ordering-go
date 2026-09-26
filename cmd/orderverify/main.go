// orderverify 是公平排序结果的独立离线校验工具。
//
// 用法：
//
//	orderverify result.json
//
// result.json 为 fairorder.OrderResult 的 JSON 表示。工具不依赖任何运行中的
// 服务，仅依据公开规则复算：排序键是否为 SHA256(payload)、顺序是否符合
// (order_key, commitment, sender, nonce) 升序、身份是否唯一。
// 校验通过退出码为 0，否则打印原因并以退出码 1 结束。
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/guoyyit/fair-transaction-ordering-go"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s result.json\n", os.Args[0])
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read result: %v\n", err)
		os.Exit(2)
	}
	var res fairorder.OrderResult
	if err := json.Unmarshal(data, &res); err != nil {
		fmt.Fprintf(os.Stderr, "parse result: %v\n", err)
		os.Exit(2)
	}
	if err := fairorder.VerifyOrderResult(&res); err != nil {
		fmt.Printf("INVALID: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("OK: round=%s transactions=%d excluded=%d\n", res.Round, len(res.Transactions), len(res.Excluded))
}
