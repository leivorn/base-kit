package admin

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	mrand "math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/leivorn/base-kit/config"
)

// 后台登录拼图滑块验证码：系统配置 login_captcha 开启时，登录前必须把拼图块拖进缺口。
//
// 流程：GET /admin/auth/captcha 拿带缺口的背景图、拼图块和拼图块的纵坐标；前端（vben 的滑块条）
// 横向拖动拼图块，POST /admin/auth/captcha/verify 提交位移，服务端比对缺口位置，通过后发一次性凭证，
// 登录时带上。
//
// 缺口位置只存在服务端。vben 自带的 SliderTranslateCaptcha 等组件在浏览器里随机缺口、在浏览器里判断
// 通过，脚本绕过页面直接调登录接口就失效了，只能借它的样式和滑块条。
//
// 拼图只能拖一次：拖错即作废、换新图，并计入来源 IP 的失败次数（见 RecordCaptchaFailure）。
// 存储在进程内存，和登录失败闸一样只面向单实例后台，重启清零可以接受。
const (
	CaptchaConfigKey = "login_captcha"

	PuzzleWidth     = 320
	PuzzleHeight    = 160
	puzzleSide      = 44 // 拼图方块边长
	puzzleKnob      = 9  // 凸起与凹口的半径
	puzzleMargin    = 3  // 拼图块四周留给描边的余量
	puzzleTolerance = 5  // 允许的横向误差（背景图像素）

	// PuzzlePieceSize 拼图块图片边长：方块 + 顶部/右侧凸起 + 余量
	PuzzlePieceSize = puzzleSide + 2*puzzleKnob - 2 + 2*puzzleMargin

	captchaTTL        = 2 * time.Minute // 拼图与通过凭证的有效期
	captchaMaxEntries = 20000           // 上限，防有人狂刷验证码接口把内存撑爆
)

// CaptchaEnabled 报告登录是否需要验证码（系统配置 login_captcha）。
//
// 配置项这一行不存在时（种子模式 off 下库里还没有这一行，或被删了还没重启）按种子的默认值：
// 生产开、开发关；存在但停用（status=0）时视为关闭，与此前一致。
func CaptchaEnabled() bool {
	if !configExists(CaptchaConfigKey) {
		return config.IsProduction()
	}
	return ConfigBool(CaptchaConfigKey)
}

// PuzzleCaptcha 一张拼图验证码。坐标都按背景图原始像素。
type PuzzleCaptcha struct {
	ID     string
	Image  string // 带缺口的背景图，JPEG data URI
	Piece  string // 拼图块，PNG data URI（透明底，边长 PuzzlePieceSize）
	PieceY int    // 拼图块顶边的纵坐标；横向从 0 开始拖
}

// GenerateCaptcha 生成一张拼图，答案（拼图块应拖到的横坐标）只留在服务端。
func GenerateCaptcha() (*PuzzleCaptcha, error) {
	bg, piece, answerX, pieceY := drawPuzzle()

	var bgBuf, pieceBuf bytes.Buffer
	if err := jpeg.Encode(&bgBuf, bg, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	if err := png.Encode(&pieceBuf, piece); err != nil {
		return nil, err
	}
	id := rand.Text()
	captchaPuzzles.set(id, strconv.Itoa(answerX))
	return &PuzzleCaptcha{
		ID:     id,
		Image:  "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(bgBuf.Bytes()),
		Piece:  "data:image/png;base64," + base64.StdEncoding.EncodeToString(pieceBuf.Bytes()),
		PieceY: pieceY,
	}, nil
}

// VerifyCaptchaSlide 校验拼图块拖到的横坐标，通过时返回一次性登录凭证。拼图校验即作废，不论对错。
func VerifyCaptchaSlide(id string, x float64) (token string, ok bool) {
	if id == "" || math.IsNaN(x) || math.IsInf(x, 0) {
		return "", false
	}
	answer, found := captchaPuzzles.take(id)
	if !found {
		return "", false
	}
	want, err := strconv.Atoi(answer)
	if err != nil || math.Abs(x-float64(want)) > puzzleTolerance {
		return "", false
	}
	token = rand.Text()
	captchaPasses.set(token, "")
	return token, true
}

// ConsumeCaptchaToken 登录时校验并作废通过凭证。空凭证一律不通过。
func ConsumeCaptchaToken(token string) bool {
	if token == "" {
		return false
	}
	_, ok := captchaPasses.take(token)
	return ok
}

// --- 存储 ---

type captchaEntry struct {
	value   string
	expires time.Time
}

type captchaStore struct {
	mu      sync.Mutex
	entries map[string]captchaEntry
	now     func() time.Time
}

var (
	captchaPuzzles = newCaptchaStore(time.Now) // 拼图 id -> 缺口横坐标
	captchaPasses  = newCaptchaStore(time.Now) // 通过凭证
)

func newCaptchaStore(now func() time.Time) *captchaStore {
	return &captchaStore{entries: map[string]captchaEntry{}, now: now}
}

func (s *captchaStore) set(id, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if len(s.entries) >= captchaMaxEntries {
		for k, e := range s.entries {
			if now.After(e.expires) {
				delete(s.entries, k)
			}
		}
		// 全都没过期说明正被狂刷：随机丢一批（map 遍历顺序随机），正常用户最多重新拖一次
		for k := range s.entries {
			if len(s.entries) < captchaMaxEntries*9/10 {
				break
			}
			delete(s.entries, k)
		}
	}
	s.entries[id] = captchaEntry{value: value, expires: now.Add(captchaTTL)}
}

// take 取出并删除，过期视为不存在。
func (s *captchaStore) take(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return "", false
	}
	delete(s.entries, id)
	if s.now().After(e.expires) {
		return "", false
	}
	return e.value, true
}

// --- 绘制 ---

// inPuzzleShape 判断点是否落在拼图块形状内，坐标相对方块左上角：方块左侧挖一个凹口，
// 顶部和右侧各一个凸起（与 vben SliderTranslateCaptcha 的形状一致）。
func inPuzzleShape(dx, dy float64) bool {
	const s, r = float64(puzzleSide), float64(puzzleKnob)
	in := dx >= 0 && dx < s && dy >= 0 && dy < s
	if in && math.Hypot(dx-(r-2), dy-s/2) < r {
		in = false
	}
	return in || math.Hypot(dx-s/2, dy-(2-r)) < r || math.Hypot(dx-(s+r-2), dy-s/2) < r
}

// drawPuzzle 画一张背景图，在随机位置挖出缺口，返回背景图、拼图块、答案横坐标与拼图块纵坐标。
func drawPuzzle() (bg, piece *image.RGBA, answerX, pieceY int) {
	bg = drawPuzzleBackground()

	// 方块左上角 (gx, gy)。拼图块图片的左上角 = (gx - 余量, gy - 顶部凸起 - 余量)，这就是答案。
	// 横向：拼图块从 0 开始拖，缺口至少离开起点一个身位；右边留出滑块按钮的宽度（前端最多拖到 宽 - 46）。
	const top = 2*puzzleKnob - 2 + puzzleMargin
	minX := PuzzlePieceSize + 10 + puzzleMargin
	maxX := PuzzleWidth - PuzzlePieceSize - 8 + puzzleMargin
	gx := minX + mrand.IntN(maxX-minX+1)
	gy := top + 2 + mrand.IntN(PuzzleHeight-puzzleSide-puzzleMargin-2-(top+2)+1)
	answerX, pieceY = gx-puzzleMargin, gy-top

	// 先按 4×4 超采样算出拼图块范围内每个像素的覆盖率，边缘抗锯齿
	n := PuzzlePieceSize
	cover := make([]float64, n*n)
	for py := range n {
		for px := range n {
			var hit int
			for sy := range 4 {
				for sx := range 4 {
					dx := float64(px-puzzleMargin) + (float64(sx)+0.5)/4
					dy := float64(py-top) + (float64(sy)+0.5)/4
					if inPuzzleShape(dx, dy) {
						hit++
					}
				}
			}
			cover[py*n+px] = float64(hit) / 16
		}
	}
	at := func(px, py int) float64 {
		if px < 0 || py < 0 || px >= n || py >= n {
			return 0
		}
		return cover[py*n+px]
	}
	// edge 表示离轮廓 2 像素以内（内外都算），用来描边
	edge := func(px, py int) bool {
		inside := at(px, py) >= 0.5
		for oy := -2; oy <= 2; oy++ {
			for ox := -2; ox <= 2; ox++ {
				if ox*ox+oy*oy <= 4 && (at(px+ox, py+oy) >= 0.5) != inside {
					return true
				}
			}
		}
		return false
	}

	piece = image.NewRGBA(image.Rect(0, 0, n, n))
	ox, oy := answerX, pieceY
	for py := range n {
		for px := range n {
			c := at(px, py)
			orig := bg.RGBAAt(ox+px, oy+py)
			onEdge := edge(px, py)
			// 拼图块：取缺口处原图，描一圈半透明白边
			if c > 0 {
				p := orig
				if onEdge {
					p = mixRGBA(p, color.RGBA{255, 255, 255, 255}, 0.65)
				}
				p.A = uint8(255 * c)
				piece.SetRGBA(px, py, premultiply(p))
			} else if onEdge {
				piece.SetRGBA(px, py, premultiply(color.RGBA{255, 255, 255, 90}))
			}
			// 缺口：压暗，边缘提亮一圈
			if c > 0 {
				g := mixRGBA(orig, color.RGBA{0, 0, 0, 255}, 0.55*c)
				if onEdge {
					g = mixRGBA(g, color.RGBA{255, 255, 255, 255}, 0.35)
				}
				bg.SetRGBA(ox+px, oy+py, g)
			}
		}
	}
	return bg, piece, answerX, pieceY
}

// puzzlePalettes 每组：天空上、天空下、山的主色。扁平插画风的配色，随机挑一组。
var puzzlePalettes = [][3]color.RGBA{
	{{0x7f, 0xa8, 0xf5, 255}, {0xc2, 0xe9, 0xfb, 255}, {0x2f, 0x5d, 0x8a, 255}},
	{{0xe0, 0xa8, 0xd8, 255}, {0xa6, 0xc1, 0xee, 255}, {0x4a, 0x3f, 0x8f, 255}},
	{{0xf6, 0xb0, 0x65, 255}, {0xfd, 0xd9, 0xa0, 255}, {0xa8, 0x4a, 0x3a, 255}},
	{{0x6c, 0xc8, 0xd8, 255}, {0xd4, 0xf5, 0xe0, 255}, {0x2e, 0x7d, 0x5b, 255}},
	{{0xfc, 0x9f, 0x8b, 255}, {0xff, 0xe0, 0xc8, 255}, {0x8a, 0x3b, 0x4f, 255}},
	{{0x8e, 0x9e, 0xf5, 255}, {0xe0, 0xc3, 0xfc, 255}, {0x35, 0x3a, 0x7a, 255}},
	{{0x5b, 0x9b, 0xd5, 255}, {0xf7, 0xd9, 0xa8, 255}, {0x3d, 0x5a, 0x6c, 255}},
	{{0x9b, 0xd4, 0xa8, 255}, {0xf0, 0xf7, 0xd0, 255}, {0x3b, 0x6e, 0x45, 255}},
}

// drawPuzzleBackground 画一张扁平风景插画：渐变天空、太阳、光斑、三层起伏的山，再叠一层细噪点
// （噪点让缺口不能靠和"干净底图"做差找出来）。
func drawPuzzleBackground() *image.RGBA {
	pal := puzzlePalettes[mrand.IntN(len(puzzlePalettes))]
	skyTop, skyBottom, hill := pal[0], pal[1], pal[2]
	img := image.NewRGBA(image.Rect(0, 0, PuzzleWidth, PuzzleHeight))

	for y := range PuzzleHeight {
		row := mixRGBA(skyTop, skyBottom, float64(y)/float64(PuzzleHeight-1))
		for x := range PuzzleWidth {
			img.SetRGBA(x, y, row)
		}
	}

	// 太阳 + 光斑：边缘柔和的圆
	sun := color.RGBA{255, 250, 235, 255}
	softCircle(img, 40+mrand.Float64()*240, 22+mrand.Float64()*30, 16+mrand.Float64()*10, sun, 0.85)
	for range 6 + mrand.IntN(6) {
		softCircle(img, mrand.Float64()*PuzzleWidth, mrand.Float64()*PuzzleHeight*0.7,
			6+mrand.Float64()*22, color.RGBA{255, 255, 255, 255}, 0.12+mrand.Float64()*0.18)
	}

	// 三层山：远处的颜色更接近天空
	for layer := range 3 {
		base := PuzzleHeight * (0.52 + 0.14*float64(layer))
		c := mixRGBA(hill, skyBottom, 0.62-0.27*float64(layer))
		a1, f1, p1 := 8+mrand.Float64()*10, 0.01+mrand.Float64()*0.015, mrand.Float64()*2*math.Pi
		a2, f2, p2 := 3+mrand.Float64()*5, 0.03+mrand.Float64()*0.03, mrand.Float64()*2*math.Pi
		for x := range PuzzleWidth {
			ridge := base - a1*math.Sin(float64(x)*f1+p1) - a2*math.Sin(float64(x)*f2+p2)
			for y := int(ridge); y < PuzzleHeight; y++ {
				cov := math.Min(1, float64(y+1)-ridge) // 山脊这一像素按覆盖率抗锯齿
				if cov <= 0 {
					continue
				}
				img.SetRGBA(x, y, mixRGBA(img.RGBAAt(x, y), c, cov))
			}
		}
	}

	for i := 0; i < len(img.Pix); i += 4 {
		d := mrand.IntN(13) - 6
		for j := range 3 {
			img.Pix[i+j] = clamp8(int(img.Pix[i+j]) + d)
		}
	}
	return img
}

func softCircle(img *image.RGBA, cx, cy, r float64, c color.RGBA, alpha float64) {
	for y := int(cy - r - 1); y <= int(cy+r+1); y++ {
		for x := int(cx - r - 1); x <= int(cx+r+1); x++ {
			if x < 0 || y < 0 || x >= PuzzleWidth || y >= PuzzleHeight {
				continue
			}
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			// 中心实、外圈 30% 渐隐
			a := alpha * math.Max(0, math.Min(1, (r-d)/(r*0.3)))
			if a > 0 {
				img.SetRGBA(x, y, mixRGBA(img.RGBAAt(x, y), c, a))
			}
		}
	}
}

func mixRGBA(a, b color.RGBA, t float64) color.RGBA {
	m := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t + 0.5) }
	return color.RGBA{m(a.R, b.R), m(a.G, b.G), m(a.B, b.B), 255}
}

// premultiply 把直通 alpha 的颜色转成 image.RGBA 要求的预乘形式。
func premultiply(c color.RGBA) color.RGBA {
	f := float64(c.A) / 255
	return color.RGBA{uint8(float64(c.R)*f + 0.5), uint8(float64(c.G)*f + 0.5), uint8(float64(c.B)*f + 0.5), c.A}
}

func clamp8(v int) uint8 {
	return uint8(min(255, max(0, v)))
}
