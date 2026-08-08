package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

type fakeBlobs struct {
	data map[string][]byte
	read []string
}

func (f *fakeBlobs) ReadBlob(_ context.Context, key string) ([]byte, error) {
	f.read = append(f.read, key)
	b, ok := f.data[key]
	if !ok {
		return nil, errors.New("not found: " + key)
	}
	return b, nil
}

const visionMapping = `{
  "body_template": { "messages": [ { "role": "user", "content": [] } ] },
  "rules": [
    {"from": "inputs.image", "to": "messages.0.content[]", "wrap": "image_url_part", "inline": true},
    {"from": "prompt", "to": "messages.0.content[]", "wrap": "text_part"}
  ]
}`

func visionInput(blobs adapter.BlobReader, refs ...adapter.InputRef) adapter.SubmitInput {
	return adapter.SubmitInput{
		Prompt: "这张图里的主体是什么颜色？",
		Model:  domain.ModelConfig{ID: "gpugeek-qwen3-vl-plus", RequestMapping: visionMapping},
		Inputs: refs,
		Blobs:  blobs,
	}
}

// 内联的整条链路：被 inline 规则引用到的槽读出字节，按 InputRef.MIME 拼成
// data URL 进请求体。上游拉不到本平台地址，这是唯一能把图喂过去的形态。
func TestRenderBodyInlinesInputAsDataURL(t *testing.T) {
	blobs := &fakeBlobs{data: map[string][]byte{"orig/a1.png": []byte("ABC")}}
	in := visionInput(blobs, adapter.InputRef{
		Slot: "image", URL: "http://127.0.0.1:8081/api/assets/a1/content",
		StorageKey: "orig/a1.png", MIME: "image/png", Bytes: 3,
	})

	body, err := RenderBody(context.Background(), nil, in)
	if err != nil {
		t.Fatal(err)
	}

	parts := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("content 应有图与文两段，实际 %d 段: %v", len(parts), parts)
	}
	got := parts[0].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if got != "data:image/png;base64,QUJD" {
		t.Fatalf("图片段不是内联 data URL: %q", got)
	}
}

// 没有 inline 规则的模型一份字节都不该读：读一份素材是一次存储往返加一份
// 内存拷贝，让走地址的多数派模型替少数派买单是实打实的浪费。
func TestRenderBodySkipsBlobReadWithoutInlineRule(t *testing.T) {
	blobs := &fakeBlobs{data: map[string][]byte{"orig/a1.png": []byte("ABC")}}
	in := visionInput(blobs, adapter.InputRef{
		Slot: "image", StorageKey: "orig/a1.png", MIME: "image/png", Bytes: 3,
	})
	in.Model.RequestMapping = `{"rules":[{"from":"inputs.image","to":"image[]","wrap":"image_url_part"}]}`

	if _, err := RenderBody(context.Background(), nil, in); err != nil {
		t.Fatal(err)
	}
	if len(blobs.read) != 0 {
		t.Fatalf("不该读字节，实际读了 %v", blobs.read)
	}
}

func TestRenderBodyInlineFailures(t *testing.T) {
	big := adapter.InputRef{Slot: "image", StorageKey: "orig/big.png", MIME: "image/png", Bytes: MaxInlineBytes + 1}
	ok := adapter.InputRef{Slot: "image", StorageKey: "orig/a1.png", MIME: "image/png", Bytes: 3}

	cases := []struct {
		name  string
		blobs adapter.BlobReader
		ref   adapter.InputRef
	}{
		// 没注入 BlobReader 时报「缺 BlobReader」，比让上游收到一个空图段好定位。
		{"没有 BlobReader", nil, ok},
		// 超限先按登记大小挡，不必为了拒绝一份大文件先把它读进内存。
		{"素材超过内联上限", &fakeBlobs{}, big},
		{"读字节失败", &fakeBlobs{}, ok},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RenderBody(context.Background(), nil, visionInput(tc.blobs, tc.ref))
			if err == nil {
				t.Fatal("期望报错，实际通过")
			}
		})
	}
}

// 渲染结果绝不含凭证：它要能原样回显给 admin 的 probe 接口。
func TestRenderBodyCarriesNoCredential(t *testing.T) {
	blobs := &fakeBlobs{data: map[string][]byte{"orig/a1.png": []byte("ABC")}}
	in := visionInput(blobs, adapter.InputRef{
		Slot: "image", StorageKey: "orig/a1.png", MIME: "image/png", Bytes: 3,
	})
	in.Credential = "sk-must-not-leak"

	body, err := RenderBody(context.Background(), nil, in)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), in.Credential) {
		t.Fatal("渲染结果里出现了凭证")
	}
}
