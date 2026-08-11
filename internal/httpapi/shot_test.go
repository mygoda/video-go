package httpapi

import "testing"

func TestParseShotReply(t *testing.T) {
	cases := []struct {
		name         string
		reply        string
		wantDesc     string
		wantDialogue string
		wantErr      bool
	}{
		{
			name:         "两段全角冒号",
			reply:        "描述：暴雨中的天台，男人握紧栏杆\n台词：你到底想怎样？",
			wantDesc:     "暴雨中的天台，男人握紧栏杆",
			wantDialogue: "你到底想怎样？",
		},
		{
			name:         "半角冒号也认",
			reply:        "描述:走廊尽头的红灯闪烁\n台词:别过来。",
			wantDesc:     "走廊尽头的红灯闪烁",
			wantDialogue: "别过来。",
		},
		{
			name:         "空镜没有台词",
			reply:        "描述：空荡的地铁站台，风吹动废报纸\n台词：",
			wantDesc:     "空荡的地铁站台，风吹动废报纸",
			wantDialogue: "",
		},
		{
			name:         "台词外层的引号被剥掉",
			reply:        "描述：她转过身\n台词：「我不会回来了」",
			wantDesc:     "她转过身",
			wantDialogue: "我不会回来了",
		},
		{
			name:         "多行描述",
			reply:        "描述：第一行\n第二行\n台词：走吧",
			wantDesc:     "第一行\n第二行",
			wantDialogue: "走吧",
		},
		{
			name:     "没有台词标记时整段当描述",
			reply:    "只有一段画面描述，模型没按格式回",
			wantDesc: "只有一段画面描述，模型没按格式回",
		},
		{
			name:    "空回复报错",
			reply:   "   \n  ",
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseShotReply(c.reply)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，得到 %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("未预期的错误：%v", err)
			}
			if got.Description != c.wantDesc {
				t.Errorf("描述 = %q，想要 %q", got.Description, c.wantDesc)
			}
			if got.Dialogue != c.wantDialogue {
				t.Errorf("台词 = %q，想要 %q", got.Dialogue, c.wantDialogue)
			}
		})
	}
}
