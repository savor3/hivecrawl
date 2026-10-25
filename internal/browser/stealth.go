package browser

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// Light anti-detection for headless Chrome. Lessons from auditing an
// anti-detect setup: every value a site can read must agree with the
// others — the user agent with the real browser version, the window with
// the screen, the timezone with the IP — and native overrides beat
// JavaScript patches wherever Chrome offers one.

// profile is what the page is told about the device.
type profile struct {
	ua               string
	meta             *emulation.UserAgentMetadata
	width, height    int64 // viewport
	screenW, screenH int64
	windowExtraH     int64 // browser UI above the page (outerHeight - innerHeight)
	mobile           bool
}

var chromeVersionRe = regexp.MustCompile(`Chrome/(\d+)(\.[\d.]+)?`)

// version returns the browser's major and full version ("155", "155.0.8059.40").
func (b *Browser) version(ctx context.Context) (string, string) {
	b.verOnce.Do(func() {
		hp, err := b.devtoolsHost(ctx)
		if err != nil {
			return
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+hp+"/json/version", nil)
		resp, err := b.http.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		var v struct {
			Browser string `json:"Browser"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&v) == nil {
			if m := chromeVersionRe.FindStringSubmatch(v.Browser); m != nil {
				b.major, b.full = m[1], m[1]+m[2]
			}
		}
	})
	if b.major == "" {
		return "155", "155.0.0.0"
	}
	return b.major, b.full
}

func (b *Browser) profile(ctx context.Context, mobile bool) profile {
	major, full := b.version(ctx)
	brands := func(v string) []*emulation.UserAgentBrandVersion {
		return []*emulation.UserAgentBrandVersion{
			{Brand: "Chromium", Version: v}, {Brand: "Google Chrome", Version: v}, {Brand: "Not_A Brand", Version: "24"},
		}
	}
	if mobile {
		return profile{
			ua: "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + major + ".0.0.0 Mobile Safari/537.36",
			meta: &emulation.UserAgentMetadata{Brands: brands(major), FullVersionList: brands(full), Platform: "Android",
				PlatformVersion: "14.0.0", Model: "Pixel 8", Mobile: true},
			width: 412, height: 839, screenW: 412, screenH: 915, mobile: true,
		}
	}
	return profile{
		ua: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + major + ".0.0.0 Safari/537.36",
		meta: &emulation.UserAgentMetadata{Brands: brands(major), FullVersionList: brands(full), Platform: "Linux",
			PlatformVersion: "6.8.0", Architecture: "x86", Bitness: "64"},
		width: 1366, height: 900, screenW: 1920, screenH: 1080, windowExtraH: 85,
	}
}

// harden applies the profile to the current tab before navigation.
func (b *Browser) harden(ctx context.Context, p profile) error {
	steps := []func() error{
		func() error {
			_, err := chromedp.Call(ctx, emulation.SetAutomationOverride, emulation.SetAutomationOverrideParams{Enabled: false})
			return err
		},
		func() error {
			_, err := chromedp.Call(ctx, emulation.SetUserAgentOverride, emulation.SetUserAgentOverrideParams{
				UserAgent: p.ua, AcceptLanguage: "en-US,en;q=0.9", Platform: platformFor(p), UserAgentMetadata: p.meta,
			})
			return err
		},
		func() error {
			// Size the real window so outer > inner like a desktop browser.
			c := chromedp.FromContext(ctx)
			if c == nil || c.Target == nil || p.mobile {
				return nil
			}
			w, err := chromedp.CallBrowser(ctx, cdpbrowser.GetWindowForTarget, cdpbrowser.GetWindowForTargetParams{TargetID: c.Target.TargetID})
			if err != nil {
				return nil // not fatal
			}
			chromedp.CallBrowser(ctx, cdpbrowser.SetWindowBounds, cdpbrowser.SetWindowBoundsParams{
				WindowID: w.WindowID, Bounds: &cdpbrowser.Bounds{Width: p.width, Height: p.height + p.windowExtraH},
			})
			return nil
		},
		func() error {
			_, err := chromedp.Call(ctx, emulation.SetDeviceMetricsOverride, emulation.SetDeviceMetricsOverrideParams{
				Width: p.width, Height: p.height, DeviceScaleFactor: map[bool]float64{true: 2.625, false: 1}[p.mobile],
				Mobile: p.mobile, ScreenWidth: p.screenW, ScreenHeight: p.screenH,
			})
			return err
		},
		func() error {
			if !p.mobile {
				return nil
			}
			_, err := chromedp.Call(ctx, emulation.SetTouchEmulationEnabled, emulation.SetTouchEmulationEnabledParams{Enabled: true, MaxTouchPoints: 5})
			return err
		},
		func() error {
			if b.Timezone == "" {
				return nil
			}
			_, err := chromedp.Call(ctx, emulation.SetTimezoneOverride, emulation.SetTimezoneOverrideParams{TimezoneID: b.Timezone})
			return err
		},
		func() error {
			_, err := chromedp.Call(ctx, page.AddScriptToEvaluateOnNewDocument, page.AddScriptToEvaluateOnNewDocumentParams{Source: stealthJS(p)})
			return err
		},
	}
	for _, s := range steps {
		if err := s(); err != nil {
			return err
		}
	}
	return nil
}

func platformFor(p profile) string {
	if p.mobile {
		return "Linux armv81"
	}
	return "Linux x86_64"
}

// stealthJS fills in what headless Chrome lacks compared with desktop
// Chrome: window.chrome, the built-in PDF plugins, a consistent
// notification permission, a real-looking GPU and outer window size.
func stealthJS(p profile) string {
	gpuVendor, gpuRenderer := "Google Inc. (Intel)", "ANGLE (Intel, Mesa Intel(R) UHD Graphics 620 (KBL GT2), OpenGL 4.6)"
	if p.mobile {
		gpuVendor, gpuRenderer = "Qualcomm", "Adreno (TM) 740"
	}
	r := strings.NewReplacer("__VENDOR__", gpuVendor, "__RENDERER__", gpuRenderer, "__EXTRA_H__", itoa(p.windowExtraH))
	return r.Replace(`(() => {
  const native = (fn, name) => {
    const s = 'function ' + name + '() { [native code] }';
    try { Object.defineProperty(fn, 'toString', { value: () => s }); } catch (e) {}
    return fn;
  };
  // Desktop Chrome has navigator.webdriver === false when not automated.
  try {
    Object.defineProperty(Navigator.prototype, 'webdriver', { get: native(function webdriver() { return false; }, 'get webdriver'), configurable: true });
  } catch (e) {}
  if (!window.chrome) {
    Object.defineProperty(window, 'chrome', { value: {
      app: { isInstalled: false, InstallState: { DISABLED: 'disabled', INSTALLED: 'installed', NOT_INSTALLED: 'not_installed' },
             RunningState: { CANNOT_RUN: 'cannot_run', READY_TO_RUN: 'ready_to_run', RUNNING: 'running' } },
      runtime: {}, csi: native(function () { return {}; }, 'csi'), loadTimes: native(function () { return {}; }, 'loadTimes'),
    }, configurable: true, writable: true });
  }
  try {
    const names = ['PDF Viewer', 'Chrome PDF Viewer', 'Chromium PDF Viewer', 'Microsoft Edge PDF Viewer', 'WebKit built-in PDF'];
    const plugins = names.map((n) => {
      const pl = Object.create(Plugin.prototype);
      Object.defineProperties(pl, { name: { value: n }, filename: { value: 'internal-pdf-viewer' },
        description: { value: 'Portable Document Format' }, length: { value: 2 } });
      return pl;
    });
    const arr = Object.create(PluginArray.prototype);
    plugins.forEach((pl, i) => Object.defineProperty(arr, i, { value: pl, enumerable: true }));
    Object.defineProperties(arr, { length: { value: plugins.length },
      item: { value: native(function item(i) { return plugins[i] || null; }, 'item') },
      namedItem: { value: native(function namedItem(n) { return plugins.find((pl) => pl.name === n) || null; }, 'namedItem') },
      refresh: { value: native(function refresh() {}, 'refresh') } });
    Object.defineProperty(Navigator.prototype, 'plugins', { get: native(function plugins() { return arr; }, 'get plugins'), configurable: true });
    Object.defineProperty(Navigator.prototype, 'pdfViewerEnabled', { get: native(function () { return true; }, 'get pdfViewerEnabled'), configurable: true });
  } catch (e) {}
  try {
    const q = Permissions.prototype.query;
    Permissions.prototype.query = native(function query(desc) {
      if (desc && desc.name === 'notifications') {
        const state = Notification.permission === 'default' ? 'prompt' : Notification.permission;
        return Promise.resolve(Object.setPrototypeOf({ state, onchange: null }, PermissionStatus.prototype));
      }
      return q.call(this, desc);
    }, 'query');
  } catch (e) {}
  const patchGL = (proto) => {
    if (!proto) return;
    const gp = proto.getParameter;
    proto.getParameter = native(function getParameter(p) {
      if (p === 37445) return '__VENDOR__';
      if (p === 37446) return '__RENDERER__';
      return gp.call(this, p);
    }, 'getParameter');
  };
  patchGL(window.WebGLRenderingContext && WebGLRenderingContext.prototype);
  patchGL(window.WebGL2RenderingContext && WebGL2RenderingContext.prototype);
  if (__EXTRA_H__ > 0) {
    try {
      Object.defineProperty(window, 'outerHeight', { get: native(function () { return window.innerHeight + __EXTRA_H__; }, 'get outerHeight'), configurable: true });
      Object.defineProperty(window, 'outerWidth', { get: native(function () { return window.innerWidth; }, 'get outerWidth'), configurable: true });
    } catch (e) {}
  }
})();`)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// DetectTimezone asks a public IP-geolocation service for the timezone of
// this node's public IP, so the browser's clock matches where the IP is.
func DetectTimezone(ctx context.Context, c *http.Client) string {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	for _, u := range []string{"https://ipapi.co/timezone/", "http://ip-api.com/line/?fields=timezone"} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		resp, err := c.Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
		resp.Body.Close()
		tz := strings.TrimSpace(string(b))
		if resp.StatusCode == 200 && strings.Contains(tz, "/") && !strings.ContainsAny(tz, " <{") {
			return tz
		}
	}
	return ""
}
