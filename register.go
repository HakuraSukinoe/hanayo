package main

import (
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"git.zxq.co/ripple/rippleapi/common"
	"git.zxq.co/ripple/schiavolib"
	"github.com/asaskevich/govalidator"
	"github.com/gin-gonic/gin"
)

func register(c *gin.Context) {
	// todo: check registrations disabled
	if c.Query("stopsign") != "1" {
		u, _ := tryBotnets(c)
		if u != "" {
			resp(c, 200, "elmo.html", &passwordResetContinueTData{
				Username: u,
				baseTemplateData: baseTemplateData{
					TitleBar:       "Elmo! Stop!",
					HeadingTitle:   "Stop!",
					KyutGrill:      "stop_sign.png",
					HeadingOnRight: true,
				},
			})
			return
		}
	}
	registerResp(c)
}

func registerSubmit(c *gin.Context) {
	// check registrations are enabled
	var enabled bool
	db.QueryRow("SELECT value_int FROM system_settings WHERE name = 'registrations_enabled'").Scan(&enabled)
	if !enabled {
		registerResp(c, errorMessage{"Sorry, it's not possible to register at the moment. Please try again later."})
		return
	}

	// check username is valid by our criteria
	username := strings.TrimSpace(c.PostForm("username"))
	if !usernameRegex.MatchString(username) {
		registerResp(c, errorMessage{"Your username must contain alphanumerical characters, spaces, or any of <code>_[]-</code>"})
		return
	}

	// check whether an username is e.g. cookiezi, shigetora, peppy, wubwoofwolf, loctav
	if in(strings.ToLower(username), forbiddenUsernames) {
		registerResp(c, errorMessage{"You're not allowed to register with that username."})
		return
	}

	// check email is valid
	if !govalidator.IsEmail(c.PostForm("email")) {
		registerResp(c, errorMessage{"Please pass a valid email address."})
		return
	}

	// passwords check (too short/too common)
	if x := validatePassword(c.PostForm("password")); x != "" {
		registerResp(c, errorMessage{x})
		return
	}

	// usernames with both _ and spaces are not allowed
	if strings.Contains(username, "_") && strings.Contains(username, " ") {
		registerResp(c, errorMessage{"An username can't contain both underscores and spaces."})
		return
	}

	// check whether username already exists
	if db.QueryRow("SELECT 1 FROM users WHERE username_safe = ?", safeUsername(username)).
		Scan(new(int)) != sql.ErrNoRows {
		registerResp(c, errorMessage{"An user with that username already exists!"})
		return
	}

	// check whether an user with that email already exists
	if db.QueryRow("SELECT 1 FROM users WHERE email = ?", c.PostForm("email")).
		Scan(new(int)) != sql.ErrNoRows {
		registerResp(c, errorMessage{"An user with that email address already exists!"})
		return
	}

	// recaptcha verify
	if config.RecaptchaPrivate != "" && !recaptchaCheck(c) {
		registerResp(c, errorMessage{"Captcha is invalid."})
		return
	}

	uMulti, criteria := tryBotnets(c)
	if criteria != "" {
		schiavo.CMs.Send(
			fmt.Sprintf(
				"User **%s** registered with the same %s as %s (%s/u/%s). **POSSIBLE MULTIACCOUNT!!!**. Waiting for ingame verification...",
				username, criteria, uMulti, config.BaseURL, url.QueryEscape(uMulti),
			),
		)
	}

	// The actual registration.
	pass, err := generatePassword(c.PostForm("password"))
	if err != nil {
		resp500(c)
		return
	}

	res, _ := db.Exec(`INSERT INTO users(username, username_safe, password_md5, salt, email, register_datetime, privileges, password_version)
							  VALUES (?,        ?,             ?,            '',   ?,     ?,                 ?,          2);`,
		username, safeUsername(username), pass, c.PostForm("email"), time.Now().Unix(), common.UserPrivilegePendingVerification)
	lid, _ := res.LastInsertId()

	db.Exec("INSERT INTO `users_stats`(id, username, user_color, user_style, ranked_score_std, playcount_std, total_score_std, ranked_score_taiko, playcount_taiko, total_score_taiko, ranked_score_ctb, playcount_ctb, total_score_ctb, ranked_score_mania, playcount_mania, total_score_mania) VALUES (?, ?, 'black', '', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0);", lid, username)

	for _, m := range []string{"std", "taiko", "ctb", "mania"} {
		var lastPosition int
		db.QueryRow("SELECT position FROM leaderboard_" + m + " ORDER BY position DESC LIMIT 1").Scan(&lastPosition)
		db.Exec("INSERT INTO leaderboard_"+m+" (position, user, v) VALUES (?, ?, ?)", lastPosition+1, lid, 0)
	}

	schiavo.CMs.Send(fmt.Sprintf("User (**%s** | %s) registered from %s", username, c.PostForm("email"), clientIP(c)))

	setYCookie(int(lid), c)
	logIP(c, int(lid))

	addMessage(c, successMessage{"You have been successfully registered on Ripple! You now need to verify your account."})
	getSession(c).Save()
	c.Redirect(302, "/register/verify?u="+strconv.Itoa(int(lid)))
}

func registerResp(c *gin.Context, messages ...message) {
	resp(c, 200, "register.html", &baseTemplateData{
		TitleBar: "Register",
		Scripts:  []string{"https://www.google.com/recaptcha/api.js"},
		Messages: messages,
		FormData: normaliseURLValues(c.Request.PostForm),
	})
}

func verifyAccount(c *gin.Context) {
	sess := getSession(c)

	i, _ := strconv.Atoi(c.Query("u"))
	y, _ := c.Cookie("y")
	err := db.QueryRow("SELECT 1 FROM identity_tokens WHERE token = ? AND userid = ?", y, i).Scan(new(int))
	if err == sql.ErrNoRows {
		addMessage(c, warningMessage{"Nope."})
		sess.Save()
		c.Redirect(302, "/")
		return
	}

	var rPrivileges uint64
	db.Get(&rPrivileges, "SELECT privileges FROM users WHERE id = ?", i)
	if common.UserPrivileges(rPrivileges)&common.UserPrivilegePendingVerification == 0 {
		addMessage(c, warningMessage{"Nope."})
		sess.Save()
		c.Redirect(302, "/")
		return
	}

	resp(c, 200, "verify.html", &baseTemplateData{
		TitleBar: "Verify account",
	})
}

func tryBotnets(c *gin.Context) (string, string) {
	var username string

	err := db.QueryRow("SELECT u.username FROM ip_user i LEFT JOIN users u ON u.id = i.userid WHERE i.ip = ?", clientIP(c)).Scan(&username)
	if err != nil {
		c.Error(err)
		return "", ""
	}
	if username != "" {
		return username, "IP"
	}

	cook, _ := c.Cookie("y")
	err = db.QueryRow("SELECT u.username FROM identity_tokens i LEFT JOIN users u ON u.id = i.userid WHERE i.token = ?",
		cook).Scan(&username)
	if err != nil {
		c.Error(err)
		return "", ""
	}
	if username != "" {
		return username, "username"
	}

	return "", ""
}

func in(s string, ss []string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

var usernameRegex = regexp.MustCompile(`^[A-Za-z0-9 _\[\]-]{2,15}$`)
var forbiddenUsernames = []string{
	"peppy",
	"rrtyui",
	"cookiezi",
	"azer",
	"loctav",
	"banchobot",
	"happystick",
	"doomsday",
	"sharingan33",
	"andrea",
	"cptnxn",
	"reimu-desu",
	"hvick225",
	"_index",
	"my aim sucks",
	"kynan",
	"rafis",
	"sayonara-bye",
	"thelewa",
	"wubwoofwolf",
	"millhioref",
	"tom94",
	"tillerino",
	"clsw",
	"spectator",
	"exgon",
	"axarious",
	"angelsim",
	"recia",
	"nara",
	"emperorpenguin83",
	"bikko",
	"xilver",
	"vettel",
	"kuu01",
	"_yu68",
	"tasuke912",
	"dusk",
	"ttobas",
	"velperk",
	"jakads",
	"jhlee0133",
	"abcdullah",
	"yuko-",
	"entozer",
	"hdhr",
	"ekoro",
	"snowwhite",
	"osuplayer111",
	"musty",
	"nero",
	"elysion",
	"ztrot",
	"koreapenguin",
	"fort",
	"asphyxia",
	"niko",
	"shigetora",
}