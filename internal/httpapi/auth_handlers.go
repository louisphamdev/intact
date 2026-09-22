package httpapi

import (
	"net/http"
	"strings"
	"time"
)

// loginPage is a single screen: six segmented boxes for the TOTP code. There is
// no password. It auto-advances, accepts a paste, and submits on the sixth
// digit. %s is the error line (a fixed internal string, never caller input).
const loginPage = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>intact — sign in</title>
<link rel="icon" href="/favicon.svg" type="image/svg+xml"><link rel="icon" href="/favicon.ico" sizes="32x32"><link rel="apple-touch-icon" href="/apple-touch-icon.png"><meta name="theme-color" content="#7c6cf6">
<script>try{const t=localStorage.getItem('intact-theme');if(t==='light'||t==='dark')document.documentElement.dataset.theme=t}catch(e){}</script>
<style>
:root{color-scheme:dark;--bg:#0c1017;--card:#141a25;--line:#232d3d;--fg:#e4e9f1;--muted:#95a0b3;--accent:#8b80f9;--mint:#5eead4;--err:#fb7185}
@media (prefers-color-scheme:light){:root:not([data-theme=dark]){color-scheme:light;--bg:#f3f5f9;--card:#fff;--line:#dfe4ec;--fg:#141a25;--muted:#526076;--accent:#5b4bdb;--mint:#0f9488;--err:#e11d48}}
:root[data-theme=light]{color-scheme:light;--bg:#f3f5f9;--card:#fff;--line:#dfe4ec;--fg:#141a25;--muted:#526076;--accent:#5b4bdb;--mint:#0f9488;--err:#e11d48}
.lang{margin-top:1.25rem;font-size:.78rem;color:var(--muted)}
.lang button{background:none;border:0;color:var(--muted);cursor:pointer;font:inherit;padding:.1rem .3rem}
.lang button[aria-pressed=true]{color:var(--accent);font-weight:600}
*{box-sizing:border-box}
body{font:15px system-ui,-apple-system,Segoe UI,sans-serif;margin:0;background:var(--bg);color:var(--fg);display:grid;place-items:center;min-height:100vh;padding:1rem}
.card{background:var(--card);border:1px solid var(--line);padding:2rem 1.75rem;border-radius:14px;width:min(23rem,94vw);text-align:center}
h1{font-size:1.6rem;font-weight:800;letter-spacing:-.03em;margin:.6rem 0 .3rem;background:linear-gradient(90deg,var(--fg) 20%,var(--mint));-webkit-background-clip:text;background-clip:text;color:transparent}
.logo{display:inline-block;filter:drop-shadow(0 6px 18px rgba(124,108,246,.45))}
p{color:var(--muted);font-size:.85rem;margin:0 0 1.5rem}
.otp{display:flex;gap:.5rem;justify-content:center}
.otp input{width:3rem;height:3.5rem;text-align:center;font-size:1.4rem;font-weight:600;color:var(--fg);
  background:var(--bg);border:1px solid var(--line);border-radius:10px;transition:border-color .15s,box-shadow .15s}
.otp input:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px rgba(139,128,249,.25)}
.otp.err input{border-color:var(--err)}
.msg{color:var(--err);font-size:.82rem;min-height:1.2em;margin-top:1rem}
@media (prefers-reduced-motion:no-preference){.otp.err{animation:shake .3s}}
@keyframes shake{25%{transform:translateX(-6px)}75%{transform:translateX(6px)}}
</style></head><body>
<div class="card">
  <span class="logo"><svg viewBox="0 0 32 32" width="52" height="52" aria-hidden="true"><defs><linearGradient id="lg" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="#7c6cf6"/><stop offset="1" stop-color="#2dd4bf"/></linearGradient></defs><rect width="32" height="32" rx="9" fill="url(#lg)"/><path d="M16 6.5 24 9.6v6.1c0 5-3.4 8.6-8 10-4.6-1.4-8-5-8-10V9.6Z" fill="none" stroke="#fff" stroke-width="2.2" stroke-linejoin="round"/><path d="m12.4 16.2 2.6 2.6 4.8-5" fill="none" stroke="#fff" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"/></svg></span>
  <h1>intact</h1>
  <p id="prompt">Enter the 6-digit code from your authenticator</p>
  <form id="f" method="post" action="/login">
    <div class="otp" id="otp">
      <input aria-label="digit 1" inputmode="numeric" pattern="[0-9]*" maxlength="1" autocomplete="one-time-code" autofocus>
      <input aria-label="digit 2" inputmode="numeric" pattern="[0-9]*" maxlength="1">
      <input aria-label="digit 3" inputmode="numeric" pattern="[0-9]*" maxlength="1">
      <input aria-label="digit 4" inputmode="numeric" pattern="[0-9]*" maxlength="1">
      <input aria-label="digit 5" inputmode="numeric" pattern="[0-9]*" maxlength="1">
      <input aria-label="digit 6" inputmode="numeric" pattern="[0-9]*" maxlength="1">
    </div>
    <input type="hidden" name="totp" id="totp">
    <div class="msg" id="msg">{{ERR}}</div>
  </form>
  <div class="lang" id="lang"><button type="button" data-l="en">English</button>·<button type="button" data-l="vi">Tiếng Việt</button></div>
</div>
<script>
// The language is the dashboard's choice, kept in this browser.
(()=>{
  const VI={'Enter the 6-digit code from your authenticator':'Nhập mã 6 số từ ứng dụng xác thực',
    'Wrong or expired code. Try again.':'Mã sai hoặc đã hết hạn. Thử lại.','intact — sign in':'intact — đăng nhập'};
  let lang='en';try{lang=localStorage.getItem('intact-lang')||((navigator.language||'').toLowerCase().startsWith('vi')?'vi':'en')}catch(e){}
  if(lang==='vi'){document.documentElement.lang='vi';
    for(const id of ['prompt','msg']){const e=document.getElementById(id);if(VI[e.textContent])e.textContent=VI[e.textContent]}
    document.title=VI[document.title]||document.title;
    document.querySelectorAll('.otp input').forEach((b,i)=>b.setAttribute('aria-label','chữ số '+(i+1)))}
  document.querySelectorAll('#lang button').forEach(b=>{b.setAttribute('aria-pressed',String(b.dataset.l===lang));
    b.onclick=()=>{try{localStorage.setItem('intact-lang',b.dataset.l)}catch(e){}location.reload()}});
})();
const boxes=[...document.querySelectorAll('.otp input')],wrap=document.getElementById('otp'),
  hidden=document.getElementById('totp'),form=document.getElementById('f');
function collect(){return boxes.map(b=>b.value).join('')}
function submit(){const v=collect();if(v.length===6){hidden.value=v;form.submit()}}
boxes.forEach((b,i)=>{
  b.addEventListener('input',()=>{b.value=b.value.replace(/\D/g,'').slice(0,1);
    if(b.value&&i<5)boxes[i+1].focus();submit()});
  b.addEventListener('keydown',e=>{if(e.key==='Backspace'&&!b.value&&i>0)boxes[i-1].focus()});
  b.addEventListener('paste',e=>{e.preventDefault();
    const d=(e.clipboardData.getData('text')||'').replace(/\D/g,'').slice(0,6);
    d.split('').forEach((c,j)=>{if(boxes[j])boxes[j].value=c});
    (boxes[Math.min(d.length,5)]||b).focus();submit()});
});
</script>
</body></html>`

func (a *api) loginForm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(renderLogin("")))
}

func (a *api) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if a.auth == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "bad form")
		return
	}
	if !a.auth.CheckCode(r.PostFormValue("totp")) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(renderLogin("Wrong or expired code. Try again.")))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    a.auth.IssueSession(),
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   a.auth.TTLSeconds(),
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *api) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: true, SameSite: http.SameSiteLaxMode, Expires: time.Unix(0, 0), MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusFound)
}

func renderLogin(errMsg string) string {
	// errMsg is a fixed internal string, never caller input, so no escaping is
	// needed. The page carries literal % in its CSS, so it is filled with a
	// string replace, never fmt.Sprintf, which would read those as verbs.
	page := loginPage
	if errMsg != "" {
		page = strings.Replace(page, `class="otp" id="otp"`, `class="otp err" id="otp"`, 1)
	}
	return strings.Replace(page, "{{ERR}}", errMsg, 1)
}
