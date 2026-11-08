package server

import (
	"encoding/base64"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

// TestCapabilityRunInputCoversArtifactRun 是**结构性**护栏：控制台能传的 run 字段
// 必须覆盖 artifact.Run 的每一个 JSON 字段。
//
// 起因是一次真实事故（#289）：handler 用独立的 `capabilityRunInput` 接 run，字段没
// 跟上 artifact.Run 新增的 workDir，于是 `run.workDir` 被 json 解码**静默丢掉**
// —— 接口返回 200、产物里没有目录、节点照旧跑在临时目录，没有任何地方报错。
// 症状是「我明明在控制台填了目录，它就是不去那儿」。
//
// 这类漏法没法靠集成测试兜住（谁会想到去断言"我传的字段还在"）。所以这里用反射
// 直接比对两份结构体的 json tag：以后给 artifact.Run 加字段，忘了加进输入结构，
// 这条就红。
func TestCapabilityRunInputCoversArtifactRun(t *testing.T) {
	tagsOf := func(v any) map[string]string {
		out := map[string]string{}
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			tag := f.Tag.Get("json")
			name, _, _ := strings.Cut(tag, ",")
			if name == "" || name == "-" {
				continue
			}
			out[name] = f.Name
		}
		return out
	}

	want := tagsOf(artifact.Run{})
	got := tagsOf(capabilityRunInput{})

	for name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("artifact.Run 有字段 json=%q，但 capabilityRunInput 没有 —— "+
				"控制台传这个字段会被**静默丢掉**（#289 的 workDir 就是这么丢的）", name)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("capabilityRunInput 有 json=%q，但 artifact.Run 没有 —— 传了也不会进产物", name)
		}
	}
}

// TestCreateCapabilityStoresRunWorkDir 端到端：控制台填的 run.workDir 必须进产物，
// 而且产物 digest 要随之变化（内容寻址：目录属于能力定义）。
func TestCreateCapabilityStoresRunWorkDir(t *testing.T) {
	env := newConsoleEnv(t)

	post := func(workDir string) (digest string, spec string) {
		t.Helper()
		run := map[string]any{
			"entrypoint": "run.sh",
			"files":      map[string]string{"run.sh": "#!/bin/bash\necho hi\n"},
		}
		if workDir != "" {
			run["workDir"] = workDir
		}
		rr := env.do(t, http.MethodPost, env.withNS("/v1/capabilities"), map[string]any{
			"name":    "cwd." + strings.ReplaceAll(strings.Trim(workDir, "/"), "/", "-"),
			"outputs": []string{},
			"run":     run,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("POST /v1/capabilities(workDir=%q) status = %d, body=%s", workDir, rr.Code, rr.Body.String())
		}
		m := decode(t, rr)
		digest, _ = m["digest"].(string)
		spec, _ = m["spec"].(string)
		if spec == "" {
			// 有的响应不带 spec，回读一次产物
			capID, _ := m["capability"].(map[string]any)["id"].(string)
			rr2 := env.do(t, http.MethodGet, env.withNS("/v1/artifacts?capabilityId="+capID), nil)
			list, _ := decode(t, rr2)["artifacts"].([]any)
			if len(list) == 0 {
				t.Fatalf("没有产物行")
			}
			spec, _ = list[0].(map[string]any)["spec"].(string)
		}
		return digest, spec
	}

	digest1, spec1 := post("/home/me/project")
	raw, err := base64.StdEncoding.DecodeString(spec1)
	if err != nil {
		t.Fatalf("spec 不是合法 base64: %v", err)
	}
	a, err := artifact.Parse(raw)
	if err != nil {
		t.Fatalf("产物解不出来: %v\n%s", err, string(raw))
	}
	if a.Spec.Run.WorkDir != "/home/me/project" {
		t.Fatalf("产物里的 run.workDir = %q, want /home/me/project（控制台传的目录被丢了？）", a.Spec.Run.WorkDir)
	}

	// 改目录 → digest 必须变（目录是产物内容的一部分）
	digest2, _ := post("/srv/app")
	if digest1 == digest2 {
		t.Fatalf("改 workDir 后 digest 没变（%s）—— 目录没进内容寻址", digest1)
	}

	// 相对路径 → 400（能力定义阶段就拦，不留一个注定跑不起来的能力）
	rr := env.do(t, http.MethodPost, env.withNS("/v1/capabilities"), map[string]any{
		"name": "cwd.bad",
		"run": map[string]any{
			"entrypoint": "run.sh",
			"files":      map[string]string{"run.sh": "#!/bin/bash\necho hi\n"},
			"workDir":    "relative/path",
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("相对 workDir status = %d, want 400, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "work_dir") {
		t.Fatalf("错误信息应提到 work_dir：%s", rr.Body.String())
	}
}

// TestListCapabilitiesExposesSpecRun 是详情页的数据来源护栏。
//
// 详情页要显示「这个能力跑多久超时、在哪个目录跑」，而这两项住在能力正文 spec.run 里
// （#289：它们属于能力定义，不属于节点配置）。列表接口投影一份**正文摘要**
// （`spec.run`，files 只有 {count, bytes}），否则前端只能显示「—」，
// 用户会以为「我填的超时没生效」。
//
// P4 起列表**不再有**旧的 `run` 投影（契约 §5）：它是 spec.run 的子集，留着就是
// 第二份形状。这条用例同时钉住「旧投影没有回来」。
func TestListCapabilitiesExposesSpecRun(t *testing.T) {
	env := newConsoleEnv(t)

	rr := env.do(t, http.MethodPost, env.withNS("/v1/capabilities"), map[string]any{
		"name":    "detail.withrun",
		"outputs": []string{"done"},
		"run": map[string]any{
			"entrypoint": "run.sh",
			"files":      map[string]string{"run.sh": "#!/bin/bash\necho hi\n"},
			"timeoutSec": 900,
			"workDir":    "/home/me/project",
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /v1/capabilities status = %d, body=%s", rr.Code, rr.Body.String())
	}

	find := func(name string) map[string]any {
		t.Helper()
		list := env.do(t, http.MethodGet, env.withNS("/v1/capabilities"), nil)
		body := decode(t, list)
		caps, _ := body["capabilities"].([]any)
		for _, c := range caps {
			m, _ := c.(map[string]any)
			if m["name"] == name {
				return m
			}
		}
		return nil
	}

	got := find("detail.withrun")
	if got == nil {
		t.Fatalf("列表里没有刚建的能力")
	}
	spec, _ := got["spec"].(map[string]any)
	if spec == nil {
		t.Fatalf("列表里的能力没有 spec：%v", got)
	}
	run, _ := spec["run"].(map[string]any)
	if run == nil {
		t.Fatalf("列表里的 spec 没有 run：%v", spec)
	}
	if run["timeoutSec"] != float64(900) {
		t.Errorf("spec.run.timeoutSec = %v, want 900（详情页要显示它）", run["timeoutSec"])
	}
	if run["workDir"] != "/home/me/project" {
		t.Errorf("spec.run.workDir = %v, want /home/me/project", run["workDir"])
	}

	// 列表必须**不带脚本正文**：files 是详情区按需懒加载的，
	// 每行都塞进来会让列表白涨几百 KB（几十个能力 × 每个几 KB 脚本）。
	files, _ := run["files"].(map[string]any)
	if files == nil {
		t.Fatalf("spec.run.files 应该是 {count, bytes} 摘要：%v", run)
	}
	if files["count"] != float64(1) || files["bytes"] == nil {
		t.Errorf("spec.run.files = %v, want {count:1, bytes:<n>}", files)
	}
	if _, exists := run["env"]; exists {
		t.Errorf("列表里的 spec.run 不该有 env 正文（详情用 /artifact 懒加载）：%v", run)
	}

	// P4 红线：旧的 `run` 投影已经删了，不能回来（两份形状迟早分叉）。
	if _, exists := got["run"]; exists {
		t.Errorf("列表里不该再有旧的 run 投影（P4 删除，改读 spec.run）：%v", got["run"])
	}

	// 失败方向：每个能力都得有名字，列表不能因为某条没有产物就少人
	if len(got) == 0 {
		t.Fatalf("能力行是空的")
	}
}
