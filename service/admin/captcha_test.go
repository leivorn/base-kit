package admin

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/leivorn/base-kit/config"
	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"
)

func decodeDataURI(t *testing.T, uri, prefix string, decode func(*bytes.Reader) (image.Image, error)) image.Image {
	t.Helper()
	raw, ok := strings.CutPrefix(uri, prefix)
	if !ok {
		t.Fatalf("不是 %s: %.40s", prefix, uri)
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	img, err := decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("解码图片: %v", err)
	}
	return img
}

func puzzleAnswer(t *testing.T, id string) int {
	t.Helper()
	x, err := strconv.Atoi(captchaPuzzles.entries[id].value)
	if err != nil {
		t.Fatalf("取答案: %v", err)
	}
	return x
}

func luminance(c interface{ RGBA() (r, g, b, a uint32) }) float64 {
	r, g, b, _ := c.RGBA()
	return 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
}

// 拼图块是缺口处的原图（透明底），背景图上同一位置被压暗；答案落在前端拖得到的范围里。
func TestGenerateCaptchaPuzzle(t *testing.T) {
	for range 20 {
		p, err := GenerateCaptcha()
		if err != nil {
			t.Fatalf("GenerateCaptcha: %v", err)
		}
		bg := decodeDataURI(t, p.Image, "data:image/jpeg;base64,", func(r *bytes.Reader) (image.Image, error) { return jpeg.Decode(r) })
		piece := decodeDataURI(t, p.Piece, "data:image/png;base64,", func(r *bytes.Reader) (image.Image, error) { return png.Decode(r) })
		if b := bg.Bounds(); b.Dx() != PuzzleWidth || b.Dy() != PuzzleHeight {
			t.Fatalf("背景图尺寸 %v", b)
		}
		if b := piece.Bounds(); b.Dx() != PuzzlePieceSize || b.Dy() != PuzzlePieceSize {
			t.Fatalf("拼图块尺寸 %v", b)
		}

		answer := puzzleAnswer(t, p.ID)
		// 拼图块从 0 开始拖，至少要离开起点一个身位；前端滑块最多拖到 宽 - 46
		if answer < PuzzlePieceSize || answer > PuzzleWidth-46 || answer+PuzzlePieceSize > PuzzleWidth {
			t.Fatalf("答案 %d 超出可拖范围", answer)
		}
		if p.PieceY < 0 || p.PieceY+PuzzlePieceSize > PuzzleHeight {
			t.Fatalf("拼图块纵坐标 %d 越界", p.PieceY)
		}

		if _, _, _, a := piece.At(0, 0).RGBA(); a != 0 {
			t.Fatal("拼图块角落应透明")
		}
		// 方块中心（左侧凹口不会挖到这里）
		cx, cy := puzzleMargin+puzzleSide*3/4, 2*puzzleKnob-2+puzzleMargin+puzzleSide/2
		if _, _, _, a := piece.At(cx, cy).RGBA(); a != 0xffff {
			t.Fatal("拼图块中心应不透明")
		}
		if luminance(bg.At(answer+cx, p.PieceY+cy)) > 0.75*luminance(piece.At(cx, cy)) {
			t.Fatal("背景图上的缺口应明显压暗，且位置与答案一致")
		}
	}
}

func TestVerifyCaptchaSlideTolerance(t *testing.T) {
	for _, tc := range []struct {
		offset float64
		pass   bool
	}{{0, true}, {puzzleTolerance, true}, {-puzzleTolerance, true}, {puzzleTolerance + 1, false}, {-puzzleTolerance - 1, false}} {
		p, err := GenerateCaptcha()
		if err != nil {
			t.Fatalf("GenerateCaptcha: %v", err)
		}
		answer := float64(puzzleAnswer(t, p.ID))
		token, ok := VerifyCaptchaSlide(p.ID, answer+tc.offset)
		if ok != tc.pass {
			t.Fatalf("偏差 %v: 通过=%v，期望 %v", tc.offset, ok, tc.pass)
		}
		// 拼图只能提交一次，不论对错
		if _, again := VerifyCaptchaSlide(p.ID, answer); again {
			t.Fatalf("偏差 %v: 拼图提交过一次后应作废", tc.offset)
		}
		if !tc.pass {
			continue
		}
		if !ConsumeCaptchaToken(token) {
			t.Fatal("通过凭证应能用于登录")
		}
		if ConsumeCaptchaToken(token) {
			t.Fatal("通过凭证只能用一次")
		}
	}
}

func TestVerifyCaptchaRejectsBadInput(t *testing.T) {
	p, err := GenerateCaptcha()
	if err != nil {
		t.Fatalf("GenerateCaptcha: %v", err)
	}
	for _, x := range []float64{math.NaN(), math.Inf(1)} {
		if _, ok := VerifyCaptchaSlide(p.ID, x); ok {
			t.Fatalf("x=%v 不应通过", x)
		}
	}
	if _, ok := VerifyCaptchaSlide("", 100); ok {
		t.Fatal("空 id 不应通过")
	}
	if _, ok := VerifyCaptchaSlide("nope", 100); ok {
		t.Fatal("不存在的 id 不应通过")
	}
	if ConsumeCaptchaToken("") || ConsumeCaptchaToken("nope") {
		t.Fatal("空或伪造的凭证不应通过")
	}
}

func TestCaptchaStoreExpiryAndCap(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := newCaptchaStore(func() time.Time { return now })

	s.set("a", "1")
	now = now.Add(captchaTTL + time.Second)
	if _, ok := s.take("a"); ok {
		t.Fatal("过期的条目不应取到")
	}
	if len(s.entries) != 0 {
		t.Fatal("取过即删除，过期的也一样")
	}

	for i := range captchaMaxEntries {
		s.set(fmt.Sprint(i), "1")
	}
	s.set("overflow", "2")
	if n := len(s.entries); n > captchaMaxEntries {
		t.Fatalf("存储超过上限: %d", n)
	}
	if got, ok := s.take("overflow"); !ok || got != "2" {
		t.Fatal("新写入的条目必须保留")
	}
}

// 拼图拖错计入来源 IP 的失败次数，和登录失败共用同一个计数：超限后两边都被锁。
func TestCaptchaFailuresShareIPLimit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newLoginLimiter(func() time.Time { return now })
	ipOnly := []loginLimitKey{ipLimitKey("1.2.3.4")}
	for range maxFailsIP - 1 {
		l.failKeys(ipOnly)
	}
	if l.lockedKeys(ipOnly) != 0 {
		t.Fatal("未达阈值不应锁定")
	}
	l.failKeys(ipOnly)
	if l.lockedKeys(ipOnly) == 0 {
		t.Fatal("拼图失败达到阈值应锁定该来源")
	}
	if l.locked("anyone", "1.2.3.4") == 0 {
		t.Fatal("该来源的登录也应被锁")
	}
	if l.locked("anyone", "5.6.7.8") != 0 {
		t.Fatal("其他来源不受影响")
	}
}

// 种子模式 off 时 sys_configs 可能还没有 login_captcha 这一行：按种子的默认值（生产开、开发关），
// 不能因为缺行就在生产上关掉验证码；行存在但停用时仍视为关闭。
func TestCaptchaEnabledFallsBackWhenRowMissing(t *testing.T) {
	useTestDB(t)
	saved := config.C
	t.Cleanup(func() { config.C = saved })

	for _, tc := range []struct {
		mode string
		row  *adminmodel.Config
		want bool
	}{
		{mode: "production", want: true},
		{mode: "development", want: false},
		{mode: "production", row: &adminmodel.Config{ConfigValue: "false", Status: 1}, want: false},
		{mode: "production", row: &adminmodel.Config{ConfigValue: "true", Status: 0}, want: false},
		{mode: "development", row: &adminmodel.Config{ConfigValue: "true", Status: 1}, want: true},
	} {
		config.C.Server.Mode = tc.mode
		store.DB.Unscoped().Where("config_key = ?", CaptchaConfigKey).Delete(&adminmodel.Config{})
		if tc.row != nil {
			tc.row.ConfigKey = CaptchaConfigKey
			if err := store.DB.Create(tc.row).Error; err != nil {
				t.Fatalf("写配置: %v", err)
			}
		}
		if got := CaptchaEnabled(); got != tc.want {
			t.Errorf("mode=%s row=%+v: CaptchaEnabled = %v, want %v", tc.mode, tc.row, got, tc.want)
		}
	}
}
