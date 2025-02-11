package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt"
	"github.com/hrfee/mediabrowser"
	"github.com/itchyny/timefmt-go"
	"github.com/lithammer/shortuuid/v3"
	"gopkg.in/ini.v1"
)

func respond(code int, message string, gc *gin.Context) {
	resp := stringResponse{}
	if code == 200 || code == 204 {
		resp.Response = message
	} else {
		resp.Error = message
	}
	gc.JSON(code, resp)
	gc.Abort()
}

func respondBool(code int, val bool, gc *gin.Context) {
	resp := boolResponse{}
	if !val {
		resp.Error = true
	} else {
		resp.Success = true
	}
	gc.JSON(code, resp)
	gc.Abort()
}

func (app *appContext) loadStrftime() {
	app.datePattern = app.config.Section("messages").Key("date_format").String()
	app.timePattern = `%H:%M`
	if val, _ := app.config.Section("messages").Key("use_24h").Bool(); !val {
		app.timePattern = `%I:%M %p`
	}
	return
}

func (app *appContext) prettyTime(dt time.Time) (date, time string) {
	date = timefmt.Format(dt, app.datePattern)
	time = timefmt.Format(dt, app.timePattern)
	return
}

func (app *appContext) formatDatetime(dt time.Time) string {
	d, t := app.prettyTime(dt)
	return d + " " + t
}

// https://stackoverflow.com/questions/36530251/time-since-with-months-and-years/36531443#36531443 THANKS
func timeDiff(a, b time.Time) (year, month, day, hour, min, sec int) {
	if a.Location() != b.Location() {
		b = b.In(a.Location())
	}
	if a.After(b) {
		a, b = b, a
	}
	y1, M1, d1 := a.Date()
	y2, M2, d2 := b.Date()

	h1, m1, s1 := a.Clock()
	h2, m2, s2 := b.Clock()

	year = int(y2 - y1)
	month = int(M2 - M1)
	day = int(d2 - d1)
	hour = int(h2 - h1)
	min = int(m2 - m1)
	sec = int(s2 - s1)

	// Normalize negative values
	if sec < 0 {
		sec += 60
		min--
	}
	if min < 0 {
		min += 60
		hour--
	}
	if hour < 0 {
		hour += 24
		day--
	}
	if day < 0 {
		// days in month:
		t := time.Date(y1, M1, 32, 0, 0, 0, 0, time.UTC)
		day += 32 - t.Day()
		month--
	}
	if month < 0 {
		month += 12
		year--
	}
	return
}

func (app *appContext) checkInvites() {
	currentTime := time.Now()
	app.storage.loadInvites()
	changed := false
	for code, data := range app.storage.invites {
		expiry := data.ValidTill
		if !currentTime.After(expiry) {
			continue
		}
		app.debug.Printf("Housekeeping: Deleting old invite %s", code)
		notify := data.Notify
		if emailEnabled && app.config.Section("notifications").Key("enabled").MustBool(false) && len(notify) != 0 {
			app.debug.Printf("%s: Expiry notification", code)
			var wait sync.WaitGroup
			for address, settings := range notify {
				if !settings["notify-expiry"] {
					continue
				}
				wait.Add(1)
				go func(addr string) {
					defer wait.Done()
					msg, err := app.email.constructExpiry(code, data, app, false)
					if err != nil {
						app.err.Printf("%s: Failed to construct expiry notification: %v", code, err)
					} else {
						// Check whether notify "address" is an email address of Jellyfin ID
						if strings.Contains(addr, "@") {
							err = app.email.send(msg, addr)
						} else {
							err = app.sendByID(msg, addr)
						}
						if err != nil {
							app.err.Printf("%s: Failed to send expiry notification: %v", code, err)
						} else {
							app.info.Printf("Sent expiry notification to %s", addr)
						}
					}
				}(address)
			}
			wait.Wait()
		}
		changed = true
		delete(app.storage.invites, code)
	}
	if changed {
		app.storage.storeInvites()
	}
}

func (app *appContext) checkInvite(code string, used bool, username string) bool {
	currentTime := time.Now()
	app.storage.loadInvites()
	changed := false
	inv, match := app.storage.invites[code]
	if !match {
		return false
	}
	expiry := inv.ValidTill
	if currentTime.After(expiry) {
		app.debug.Printf("Housekeeping: Deleting old invite %s", code)
		notify := inv.Notify
		if emailEnabled && app.config.Section("notifications").Key("enabled").MustBool(false) && len(notify) != 0 {
			app.debug.Printf("%s: Expiry notification", code)
			var wait sync.WaitGroup
			for address, settings := range notify {
				if !settings["notify-expiry"] {
					continue
				}
				wait.Add(1)
				go func(addr string) {
					defer wait.Done()
					msg, err := app.email.constructExpiry(code, inv, app, false)
					if err != nil {
						app.err.Printf("%s: Failed to construct expiry notification: %v", code, err)
					} else {
						// Check whether notify "address" is an email address of Jellyfin ID
						if strings.Contains(addr, "@") {
							err = app.email.send(msg, addr)
						} else {
							err = app.sendByID(msg, addr)
						}
						if err != nil {
							app.err.Printf("%s: Failed to send expiry notification: %v", code, err)
						} else {
							app.info.Printf("Sent expiry notification to %s", addr)
						}
					}
				}(address)
			}
			wait.Wait()
		}
		changed = true
		match = false
		delete(app.storage.invites, code)
	} else if used {
		changed = true
		del := false
		newInv := inv
		if newInv.RemainingUses == 1 {
			del = true
			delete(app.storage.invites, code)
		} else if newInv.RemainingUses != 0 {
			// 0 means infinite i guess?
			newInv.RemainingUses--
		}
		newInv.UsedBy = append(newInv.UsedBy, []string{username, strconv.FormatInt(currentTime.Unix(), 10)})
		if !del {
			app.storage.invites[code] = newInv
		}
	}
	if changed {
		app.storage.storeInvites()
	}
	return match
}

func (app *appContext) getOmbiUser(jfID string) (map[string]interface{}, int, error) {
	ombiUsers, code, err := app.ombi.GetUsers()
	if err != nil || code != 200 {
		return nil, code, err
	}
	jfUser, code, err := app.jf.UserByID(jfID, false)
	if err != nil || code != 200 {
		return nil, code, err
	}
	username := jfUser.Name
	email := ""
	if e, ok := app.storage.emails[jfID]; ok {
		email = e.Addr
	}
	for _, ombiUser := range ombiUsers {
		ombiAddr := ""
		if a, ok := ombiUser["emailAddress"]; ok && a != nil {
			ombiAddr = a.(string)
		}
		if ombiUser["userName"].(string) == username || (ombiAddr == email && email != "") {
			return ombiUser, code, err
		}
	}
	return nil, 400, fmt.Errorf("Couldn't find user")
}

// Routes from now on!

// @Summary Creates a new Jellyfin user without an invite.
// @Produce json
// @Param newUserDTO body newUserDTO true "New user request object"
// @Success 200
// @Router /users [post]
// @Security Bearer
// @tags Users
func (app *appContext) NewUserAdmin(gc *gin.Context) {
	respondUser := func(code int, user, email bool, msg string, gc *gin.Context) {
		resp := newUserResponse{
			User:  user,
			Email: email,
			Error: msg,
		}
		gc.JSON(code, resp)
		gc.Abort()
	}
	var req newUserDTO
	gc.BindJSON(&req)
	existingUser, _, _ := app.jf.UserByName(req.Username, false)
	if existingUser.Name != "" {
		msg := fmt.Sprintf("User already exists named %s", req.Username)
		app.info.Printf("%s New user failed: %s", req.Username, msg)
		respondUser(401, false, false, msg, gc)
		return
	}
	user, status, err := app.jf.NewUser(req.Username, req.Password)
	if !(status == 200 || status == 204) || err != nil {
		app.err.Printf("%s New user failed (%d): %v", req.Username, status, err)
		respondUser(401, false, false, err.Error(), gc)
		return
	}
	id := user.ID
	if app.storage.policy.BlockedTags != nil {
		status, err = app.jf.SetPolicy(id, app.storage.policy)
		if !(status == 200 || status == 204 || err == nil) {
			app.err.Printf("%s: Failed to set user policy (%d): %v", req.Username, status, err)
		}
	}
	if app.storage.configuration.GroupedFolders != nil && len(app.storage.displayprefs) != 0 {
		status, err = app.jf.SetConfiguration(id, app.storage.configuration)
		if (status == 200 || status == 204) && err == nil {
			status, err = app.jf.SetDisplayPreferences(id, app.storage.displayprefs)
		}
		if !((status == 200 || status == 204) && err == nil) {
			app.err.Printf("%s: Failed to set configuration template (%d): %v", req.Username, status, err)
		}
	}
	app.jf.CacheExpiry = time.Now()
	if emailEnabled {
		app.storage.emails[id] = EmailAddress{Addr: req.Email, Contact: true}
		app.storage.storeEmails()
	}
	if app.config.Section("ombi").Key("enabled").MustBool(false) {
		app.storage.loadOmbiTemplate()
		if len(app.storage.ombi_template) != 0 {
			errors, code, err := app.ombi.NewUser(req.Username, req.Password, req.Email, app.storage.ombi_template)
			if err != nil || code != 200 {
				app.err.Printf("Failed to create Ombi user (%d): %v", code, err)
				app.debug.Printf("Errors reported by Ombi: %s", strings.Join(errors, ", "))
			} else {
				app.info.Println("Created Ombi user")
			}
		}
	}
	if emailEnabled && app.config.Section("welcome_email").Key("enabled").MustBool(false) && req.Email != "" {
		app.debug.Printf("%s: Sending welcome email to %s", req.Username, req.Email)
		msg, err := app.email.constructWelcome(req.Username, time.Time{}, app, false)
		if err != nil {
			app.err.Printf("%s: Failed to construct welcome email: %v", req.Username, err)
			respondUser(500, true, false, err.Error(), gc)
			return
		} else if err := app.email.send(msg, req.Email); err != nil {
			app.err.Printf("%s: Failed to send welcome email: %v", req.Username, err)
			respondUser(500, true, false, err.Error(), gc)
			return
		} else {
			app.info.Printf("%s: Sent welcome email to %s", req.Username, req.Email)
		}
	}
	respondUser(200, true, true, "", gc)
}

type errorFunc func(gc *gin.Context)

// Used on the form & when a users email has been confirmed.
func (app *appContext) newUser(req newUserDTO, confirmed bool) (f errorFunc, success bool) {
	existingUser, _, _ := app.jf.UserByName(req.Username, false)
	if existingUser.Name != "" {
		f = func(gc *gin.Context) {
			msg := fmt.Sprintf("User %s already exists", req.Username)
			app.info.Printf("%s: New user failed: %s", req.Code, msg)
			respond(401, "errorUserExists", gc)
		}
		success = false
		return
	}
	var discordUser DiscordUser
	discordVerified := false
	if discordEnabled {
		if req.DiscordPIN == "" {
			if app.config.Section("discord").Key("required").MustBool(false) {
				f = func(gc *gin.Context) {
					app.debug.Printf("%s: New user failed: Discord verification not completed", req.Code)
					respond(401, "errorDiscordVerification", gc)
				}
				success = false
				return
			}
		} else {
			discordUser, discordVerified = app.discord.verifiedTokens[req.DiscordPIN]
			if !discordVerified {
				f = func(gc *gin.Context) {
					app.debug.Printf("%s: New user failed: Discord PIN was invalid", req.Code)
					respond(401, "errorInvalidPIN", gc)
				}
				success = false
				return
			}
			err := app.discord.ApplyRole(discordUser.ID)
			if err != nil {
				f = func(gc *gin.Context) {
					app.err.Printf("%s: New user failed: Failed to set member role: %v", req.Code, err)
					respond(401, "error", gc)
				}
				success = false
				return
			}
		}
	}
	var matrixUser MatrixUser
	matrixVerified := false
	if matrixEnabled {
		if req.MatrixPIN == "" {
			if app.config.Section("matrix").Key("required").MustBool(false) {
				f = func(gc *gin.Context) {
					app.debug.Printf("%s: New user failed: Matrix verification not completed", req.Code)
					respond(401, "errorMatrixVerification", gc)
				}
				success = false
				return
			}
		} else {
			user, ok := app.matrix.tokens[req.MatrixPIN]
			if !ok || !user.Verified {
				matrixVerified = false
				f = func(gc *gin.Context) {
					app.debug.Printf("%s: New user failed: Matrix PIN was invalid", req.Code)
					respond(401, "errorInvalidPIN", gc)
				}
				success = false
				return
			}
			matrixVerified = user.Verified
			matrixUser = *user.User

		}
	}
	telegramTokenIndex := -1
	if telegramEnabled {
		if req.TelegramPIN == "" {
			if app.config.Section("telegram").Key("required").MustBool(false) {
				f = func(gc *gin.Context) {
					app.debug.Printf("%s: New user failed: Telegram verification not completed", req.Code)
					respond(401, "errorTelegramVerification", gc)
				}
				success = false
				return
			}
		} else {
			for i, v := range app.telegram.verifiedTokens {
				if v.Token == req.TelegramPIN {
					telegramTokenIndex = i
					break
				}
			}
			if telegramTokenIndex == -1 {
				f = func(gc *gin.Context) {
					app.debug.Printf("%s: New user failed: Telegram PIN was invalid", req.Code)
					respond(401, "errorInvalidPIN", gc)
				}
				success = false
				return
			}
		}
	}
	if emailEnabled && app.config.Section("email_confirmation").Key("enabled").MustBool(false) && !confirmed {
		claims := jwt.MapClaims{
			"valid":       true,
			"invite":      req.Code,
			"email":       req.Email,
			"username":    req.Username,
			"password":    req.Password,
			"telegramPIN": req.TelegramPIN,
			"exp":         time.Now().Add(time.Hour * 12).Unix(),
			"type":        "confirmation",
		}
		tk := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		key, err := tk.SignedString([]byte(os.Getenv("JFA_SECRET")))
		if err != nil {
			f = func(gc *gin.Context) {
				app.info.Printf("Failed to generate confirmation token: %v", err)
				respond(500, "errorUnknown", gc)
			}
			success = false
			return
		}
		inv := app.storage.invites[req.Code]
		inv.Keys = append(inv.Keys, key)
		app.storage.invites[req.Code] = inv
		app.storage.storeInvites()
		f = func(gc *gin.Context) {
			app.debug.Printf("%s: Email confirmation required", req.Code)
			respond(401, "confirmEmail", gc)
			msg, err := app.email.constructConfirmation(req.Code, req.Username, key, app, false)
			if err != nil {
				app.err.Printf("%s: Failed to construct confirmation email: %v", req.Code, err)
			} else if err := app.email.send(msg, req.Email); err != nil {
				app.err.Printf("%s: Failed to send user confirmation email: %v", req.Code, err)
			} else {
				app.info.Printf("%s: Sent user confirmation email to \"%s\"", req.Code, req.Email)
			}
		}
		success = false
		return
	}

	user, status, err := app.jf.NewUser(req.Username, req.Password)
	if !(status == 200 || status == 204) || err != nil {
		f = func(gc *gin.Context) {
			app.err.Printf("%s New user failed (%d): %v", req.Code, status, err)
			respond(401, app.storage.lang.Admin[app.storage.lang.chosenAdminLang].Notifications.get("errorUnknown"), gc)
		}
		success = false
		return
	}
	app.storage.loadProfiles()
	invite := app.storage.invites[req.Code]
	app.checkInvite(req.Code, true, req.Username)
	if emailEnabled && app.config.Section("notifications").Key("enabled").MustBool(false) {
		for address, settings := range invite.Notify {
			if settings["notify-creation"] {
				go func() {
					msg, err := app.email.constructCreated(req.Code, req.Username, req.Email, invite, app, false)
					if err != nil {
						app.err.Printf("%s: Failed to construct user creation notification: %v", req.Code, err)
					} else {
						// Check whether notify "address" is an email address of Jellyfin ID
						if strings.Contains(address, "@") {
							err = app.email.send(msg, address)
						} else {
							err = app.sendByID(msg, address)
						}
						if err != nil {
							app.err.Printf("%s: Failed to send user creation notification: %v", req.Code, err)
						} else {
							app.info.Printf("Sent user creation notification to %s", address)
						}
					}
				}()
			}
		}
	}
	id := user.ID
	var profile Profile
	if invite.Profile != "" {
		app.debug.Printf("Applying settings from profile \"%s\"", invite.Profile)
		var ok bool
		profile, ok = app.storage.profiles[invite.Profile]
		if !ok {
			profile = app.storage.profiles["Default"]
		}
		if profile.Policy.BlockedTags != nil {
			app.debug.Printf("Applying policy from profile \"%s\"", invite.Profile)
			status, err = app.jf.SetPolicy(id, profile.Policy)
			if !((status == 200 || status == 204) && err == nil) {
				app.err.Printf("%s: Failed to set user policy (%d): %v", req.Code, status, err)
			}
		}
		if profile.Configuration.GroupedFolders != nil && len(profile.Displayprefs) != 0 {
			app.debug.Printf("Applying homescreen from profile \"%s\"", invite.Profile)
			status, err = app.jf.SetConfiguration(id, profile.Configuration)
			if (status == 200 || status == 204) && err == nil {
				status, err = app.jf.SetDisplayPreferences(id, profile.Displayprefs)
			}
			if !((status == 200 || status == 204) && err == nil) {
				app.err.Printf("%s: Failed to set configuration template (%d): %v", req.Code, status, err)
			}
		}
	}
	// if app.config.Section("password_resets").Key("enabled").MustBool(false) {
	if req.Email != "" {
		app.storage.emails[id] = EmailAddress{Addr: req.Email, Contact: true}
		app.storage.storeEmails()
	}
	expiry := time.Time{}
	if invite.UserExpiry {
		app.storage.usersLock.Lock()
		defer app.storage.usersLock.Unlock()
		expiry = time.Now().AddDate(0, invite.UserMonths, invite.UserDays).Add(time.Duration((60*invite.UserHours)+invite.UserMinutes) * time.Minute)
		app.storage.users[id] = expiry
		if err := app.storage.storeUsers(); err != nil {
			app.err.Printf("Failed to store user duration: %v", err)
		}
	}
	if discordEnabled && discordVerified {
		discordUser.Contact = req.DiscordContact
		if app.storage.discord == nil {
			app.storage.discord = map[string]DiscordUser{}
		}
		app.storage.discord[user.ID] = discordUser
		if err := app.storage.storeDiscordUsers(); err != nil {
			app.err.Printf("Failed to store Discord users: %v", err)
		} else {
			delete(app.discord.verifiedTokens, req.DiscordPIN)
		}
	}
	if telegramEnabled && telegramTokenIndex != -1 {
		tgToken := app.telegram.verifiedTokens[telegramTokenIndex]
		tgUser := TelegramUser{
			ChatID:   tgToken.ChatID,
			Username: tgToken.Username,
			Contact:  req.TelegramContact,
		}
		if lang, ok := app.telegram.languages[tgToken.ChatID]; ok {
			tgUser.Lang = lang
		}
		if app.storage.telegram == nil {
			app.storage.telegram = map[string]TelegramUser{}
		}
		app.storage.telegram[user.ID] = tgUser
		if err := app.storage.storeTelegramUsers(); err != nil {
			app.err.Printf("Failed to store Telegram users: %v", err)
		} else {
			app.telegram.verifiedTokens[len(app.telegram.verifiedTokens)-1], app.telegram.verifiedTokens[telegramTokenIndex] = app.telegram.verifiedTokens[telegramTokenIndex], app.telegram.verifiedTokens[len(app.telegram.verifiedTokens)-1]
			app.telegram.verifiedTokens = app.telegram.verifiedTokens[:len(app.telegram.verifiedTokens)-1]
		}
	}
	if invite.Profile != "" && app.config.Section("ombi").Key("enabled").MustBool(false) {
		if profile.Ombi != nil && len(profile.Ombi) != 0 {
			template := profile.Ombi
			errors, code, err := app.ombi.NewUser(req.Username, req.Password, req.Email, template)
			if err != nil || code != 200 {
				app.info.Printf("Failed to create Ombi user (%d): %s", code, err)
				app.debug.Printf("Errors reported by Ombi: %s", strings.Join(errors, ", "))
			} else {
				app.info.Println("Created Ombi user")
				if (discordEnabled && discordVerified) || (telegramEnabled && telegramTokenIndex != -1) {
					ombiUser, status, err := app.getOmbiUser(id)
					if status != 200 || err != nil {
						app.err.Printf("Failed to get Ombi user (%d): %v", status, err)
					} else {
						dID := ""
						tUser := ""
						if discordEnabled && discordVerified {
							dID = discordUser.ID
						}
						if telegramEnabled && telegramTokenIndex != -1 {
							tUser = app.storage.telegram[user.ID].Username
						}
						resp, status, err := app.ombi.SetNotificationPrefs(ombiUser, dID, tUser)
						if !(status == 200 || status == 204) || err != nil {
							app.err.Printf("Failed to link Telegram/Discord to Ombi (%d): %v", status, err)
							app.debug.Printf("Response: %v", resp)
						}
					}
				}
			}
		} else {
			app.debug.Printf("Skipping Ombi: Profile \"%s\" was empty", invite.Profile)
		}
	}
	if matrixVerified {
		matrixUser.Contact = req.MatrixContact
		delete(app.matrix.tokens, req.MatrixPIN)
		if app.storage.matrix == nil {
			app.storage.matrix = map[string]MatrixUser{}
		}
		app.storage.matrix[user.ID] = matrixUser
		if err := app.storage.storeMatrixUsers(); err != nil {
			app.err.Printf("Failed to store Matrix users: %v", err)
		}
	}
	if (emailEnabled && app.config.Section("welcome_email").Key("enabled").MustBool(false) && req.Email != "") || telegramTokenIndex != -1 || discordVerified {
		name := app.getAddressOrName(user.ID)
		app.debug.Printf("%s: Sending welcome message to %s", req.Username, name)
		msg, err := app.email.constructWelcome(req.Username, expiry, app, false)
		if err != nil {
			app.err.Printf("%s: Failed to construct welcome message: %v", req.Username, err)
		} else if err := app.sendByID(msg, user.ID); err != nil {
			app.err.Printf("%s: Failed to send welcome message: %v", req.Username, err)
		} else {
			app.info.Printf("%s: Sent welcome message to \"%s\"", req.Username, name)
		}
	}
	app.jf.CacheExpiry = time.Now()
	success = true
	return
}

// @Summary Creates a new Jellyfin user via invite code
// @Produce json
// @Param newUserDTO body newUserDTO true "New user request object"
// @Success 200 {object} PasswordValidation
// @Failure 400 {object} PasswordValidation
// @Router /newUser [post]
// @tags Users
func (app *appContext) NewUser(gc *gin.Context) {
	var req newUserDTO
	gc.BindJSON(&req)
	app.debug.Printf("%s: New user attempt", req.Code)
	if app.config.Section("captcha").Key("enabled").MustBool(false) && !app.verifyCaptcha(req.Code, req.CaptchaID, req.CaptchaText) {
		app.info.Printf("%s: New user failed: Captcha Incorrect", req.Code)
		respond(400, "errorCaptcha", gc)
		return
	}
	if !app.checkInvite(req.Code, false, "") {
		app.info.Printf("%s New user failed: invalid code", req.Code)
		respond(401, "errorInvalidCode", gc)
		return
	}
	validation := app.validator.validate(req.Password)
	valid := true
	for _, val := range validation {
		if !val {
			valid = false
		}
	}
	if !valid {
		// 200 bcs idk what i did in js
		app.info.Printf("%s: New user failed: Invalid password", req.Code)
		gc.JSON(200, validation)
		return
	}
	if emailEnabled && app.config.Section("email").Key("required").MustBool(false) && !strings.Contains(req.Email, "@") {
		app.info.Printf("%s: New user failed: Email Required", req.Code)
		respond(400, "errorNoEmail", gc)
		return
	}
	f, success := app.newUser(req, false)
	if !success {
		f(gc)
		return
	}
	code := 200
	for _, val := range validation {
		if !val {
			code = 400
		}
	}
	gc.JSON(code, validation)
}

// @Summary Enable/Disable a list of users, optionally notifying them why.
// @Produce json
// @Param enableDisableUserDTO body enableDisableUserDTO true "User enable/disable request object"
// @Success 200 {object} boolResponse
// @Failure 400 {object} stringResponse
// @Failure 500 {object} errorListDTO "List of errors"
// @Router /users/enable [post]
// @Security Bearer
// @tags Users
func (app *appContext) EnableDisableUsers(gc *gin.Context) {
	var req enableDisableUserDTO
	gc.BindJSON(&req)
	errors := errorListDTO{
		"GetUser":   map[string]string{},
		"SetPolicy": map[string]string{},
	}
	sendMail := messagesEnabled
	var msg *Message
	var err error
	if sendMail {
		if req.Enabled {
			msg, err = app.email.constructEnabled(req.Reason, app, false)
		} else {
			msg, err = app.email.constructDisabled(req.Reason, app, false)
		}
		if err != nil {
			app.err.Printf("Failed to construct account enabled/disabled emails: %v", err)
			sendMail = false
		}
	}
	for _, userID := range req.Users {
		user, status, err := app.jf.UserByID(userID, false)
		if status != 200 || err != nil {
			errors["GetUser"][userID] = fmt.Sprintf("%d %v", status, err)
			app.err.Printf("Failed to get user \"%s\" (%d): %v", userID, status, err)
			continue
		}
		user.Policy.IsDisabled = !req.Enabled
		status, err = app.jf.SetPolicy(userID, user.Policy)
		if !(status == 200 || status == 204) || err != nil {
			errors["SetPolicy"][userID] = fmt.Sprintf("%d %v", status, err)
			app.err.Printf("Failed to set policy for user \"%s\" (%d): %v", userID, status, err)
			continue
		}
		if sendMail && req.Notify {
			if err := app.sendByID(msg, userID); err != nil {
				app.err.Printf("Failed to send account enabled/disabled email: %v", err)
				continue
			}
		}
	}
	app.jf.CacheExpiry = time.Now()
	if len(errors["GetUser"]) != 0 || len(errors["SetPolicy"]) != 0 {
		gc.JSON(500, errors)
		return
	}
	respondBool(200, true, gc)
}

// @Summary Delete a list of users, optionally notifying them why.
// @Produce json
// @Param deleteUserDTO body deleteUserDTO true "User deletion request object"
// @Success 200 {object} boolResponse
// @Failure 400 {object} stringResponse
// @Failure 500 {object} errorListDTO "List of errors"
// @Router /users [delete]
// @Security Bearer
// @tags Users
func (app *appContext) DeleteUsers(gc *gin.Context) {
	var req deleteUserDTO
	gc.BindJSON(&req)
	errors := map[string]string{}
	ombiEnabled := app.config.Section("ombi").Key("enabled").MustBool(false)
	sendMail := messagesEnabled
	var msg *Message
	var err error
	if sendMail {
		msg, err = app.email.constructDeleted(req.Reason, app, false)
		if err != nil {
			app.err.Printf("Failed to construct account deletion emails: %v", err)
			sendMail = false
		}
	}
	for _, userID := range req.Users {
		if ombiEnabled {
			ombiUser, code, err := app.getOmbiUser(userID)
			if code == 200 && err == nil {
				if id, ok := ombiUser["id"]; ok {
					status, err := app.ombi.DeleteUser(id.(string))
					if err != nil || status != 200 {
						app.err.Printf("Failed to delete ombi user (%d): %v", status, err)
						errors[userID] = fmt.Sprintf("Ombi: %d %v, ", status, err)
					}
				}
			}
		}
		status, err := app.jf.DeleteUser(userID)
		if !(status == 200 || status == 204) || err != nil {
			msg := fmt.Sprintf("%d: %v", status, err)
			if _, ok := errors[userID]; !ok {
				errors[userID] = msg
			} else {
				errors[userID] += msg
			}
		}
		if sendMail && req.Notify {
			if err := app.sendByID(msg, userID); err != nil {
				app.err.Printf("Failed to send account deletion email: %v", err)
			}
		}
	}
	app.jf.CacheExpiry = time.Now()
	if len(errors) == len(req.Users) {
		respondBool(500, false, gc)
		app.err.Printf("Account deletion failed: %s", errors[req.Users[0]])
		return
	} else if len(errors) != 0 {
		gc.JSON(500, errors)
		return
	}
	respondBool(200, true, gc)
}

// @Summary Extend time before the user(s) expiry, or create and expiry if it doesn't exist.
// @Produce json
// @Param extendExpiryDTO body extendExpiryDTO true "Extend expiry object"
// @Success 200 {object} boolResponse
// @Failure 400 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Router /users/extend [post]
// @tags Users
func (app *appContext) ExtendExpiry(gc *gin.Context) {
	var req extendExpiryDTO
	gc.BindJSON(&req)
	app.info.Printf("Expiry extension requested for %d user(s)", len(req.Users))
	if req.Months <= 0 && req.Days <= 0 && req.Hours <= 0 && req.Minutes <= 0 {
		respondBool(400, false, gc)
		return
	}
	app.storage.usersLock.Lock()
	defer app.storage.usersLock.Unlock()
	for _, id := range req.Users {
		if expiry, ok := app.storage.users[id]; ok {
			app.storage.users[id] = expiry.AddDate(0, req.Months, req.Days).Add(time.Duration(((60 * req.Hours) + req.Minutes)) * time.Minute)
			app.debug.Printf("Expiry extended for \"%s\"", id)
		} else {
			app.storage.users[id] = time.Now().AddDate(0, req.Months, req.Days).Add(time.Duration(((60 * req.Hours) + req.Minutes)) * time.Minute)
			app.debug.Printf("Created expiry for \"%s\"", id)
		}
	}
	if err := app.storage.storeUsers(); err != nil {
		app.err.Printf("Failed to store user duration: %v", err)
		respondBool(500, false, gc)
		return
	}
	respondBool(204, true, gc)
}

// @Summary Send an announcement via email to a given list of users.
// @Produce json
// @Param announcementDTO body announcementDTO true "Announcement request object"
// @Success 200 {object} boolResponse
// @Failure 400 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Router /users/announce [post]
// @Security Bearer
// @tags Users
func (app *appContext) Announce(gc *gin.Context) {
	var req announcementDTO
	gc.BindJSON(&req)
	if !messagesEnabled {
		respondBool(400, false, gc)
		return
	}
	// Generally, we only need to construct once. If {username} is included, however, this needs to be done for each user.
	unique := strings.Contains(req.Message, "{username}")
	if unique {
		for _, userID := range req.Users {
			user, status, err := app.jf.UserByID(userID, false)
			if status != 200 || err != nil {
				app.err.Printf("Failed to get user with ID \"%s\" (%d): %v", userID, status, err)
				continue
			}
			msg, err := app.email.constructTemplate(req.Subject, req.Message, app, user.Name)
			if err != nil {
				app.err.Printf("Failed to construct announcement message: %v", err)
				respondBool(500, false, gc)
				return
			} else if err := app.sendByID(msg, userID); err != nil {
				app.err.Printf("Failed to send announcement message: %v", err)
				respondBool(500, false, gc)
				return
			}
		}
	} else {
		msg, err := app.email.constructTemplate(req.Subject, req.Message, app)
		if err != nil {
			app.err.Printf("Failed to construct announcement messages: %v", err)
			respondBool(500, false, gc)
			return
		} else if err := app.sendByID(msg, req.Users...); err != nil {
			app.err.Printf("Failed to send announcement messages: %v", err)
			respondBool(500, false, gc)
			return
		}
	}
	app.info.Println("Sent announcement messages")
	respondBool(200, true, gc)
}

// @Summary Save an announcement as a template for use or editing later.
// @Produce json
// @Param announcementTemplate body announcementTemplate true "Announcement request object"
// @Success 200 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Router /users/announce/template [post]
// @Security Bearer
// @tags Users
func (app *appContext) SaveAnnounceTemplate(gc *gin.Context) {
	var req announcementTemplate
	gc.BindJSON(&req)
	if !messagesEnabled {
		respondBool(400, false, gc)
		return
	}
	app.storage.announcements[req.Name] = req
	if err := app.storage.storeAnnouncements(); err != nil {
		respondBool(500, false, gc)
		app.err.Printf("Failed to store announcement templates: %v", err)
		return
	}
	respondBool(200, true, gc)
}

// @Summary Save an announcement as a template for use or editing later.
// @Produce json
// @Success 200 {object} getAnnouncementsDTO
// @Router /users/announce/template [get]
// @Security Bearer
// @tags Users
func (app *appContext) GetAnnounceTemplates(gc *gin.Context) {
	resp := &getAnnouncementsDTO{make([]string, len(app.storage.announcements))}
	i := 0
	for name := range app.storage.announcements {
		resp.Announcements[i] = name
		i++
	}
	gc.JSON(200, resp)
}

// @Summary Get an announcement template.
// @Produce json
// @Success 200 {object} announcementTemplate
// @Failure 400 {object} boolResponse
// @Param name path string true "name of template"
// @Router /users/announce/template/{name} [get]
// @Security Bearer
// @tags Users
func (app *appContext) GetAnnounceTemplate(gc *gin.Context) {
	name := gc.Param("name")
	if announcement, ok := app.storage.announcements[name]; ok {
		gc.JSON(200, announcement)
		return
	}
	respondBool(400, false, gc)
}

// @Summary Delete an announcement template.
// @Produce json
// @Success 200 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Param name path string true "name of template"
// @Router /users/announce/template/{name} [delete]
// @Security Bearer
// @tags Users
func (app *appContext) DeleteAnnounceTemplate(gc *gin.Context) {
	name := gc.Param("name")
	delete(app.storage.announcements, name)
	if err := app.storage.storeAnnouncements(); err != nil {
		respondBool(500, false, gc)
		app.err.Printf("Failed to store announcement templates: %v", err)
		return
	}
	respondBool(200, false, gc)
}

// @Summary Generate password reset links for a list of users, sending the links to them if possible.
// @Produce json
// @Param AdminPasswordResetDTO body AdminPasswordResetDTO true "List of user IDs"
// @Success 204 {object} boolResponse
// @Success 200 {object} AdminPasswordResetRespDTO
// @Failure 400 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Router /users/password-reset [post]
// @Security Bearer
// @tags Users
func (app *appContext) AdminPasswordReset(gc *gin.Context) {
	var req AdminPasswordResetDTO
	gc.BindJSON(&req)
	if req.Users == nil || len(req.Users) == 0 {
		app.debug.Println("Ignoring empty request for PWR")
		respondBool(400, false, gc)
		return
	}
	linkCount := 0
	var pwr InternalPWR
	var err error
	resp := AdminPasswordResetRespDTO{}
	for _, id := range req.Users {
		pwr, err = app.GenInternalReset(id)
		if err != nil {
			app.err.Printf("Failed to get user from Jellyfin: %v", err)
			respondBool(500, false, gc)
			return
		}
		if app.internalPWRs == nil {
			app.internalPWRs = map[string]InternalPWR{}
		}
		app.internalPWRs[pwr.PIN] = pwr
		sendAddress := app.getAddressOrName(id)
		if sendAddress == "" || len(req.Users) == 1 {
			resp.Link, err = app.GenResetLink(pwr.PIN)
			linkCount++
			if sendAddress == "" {
				resp.Manual = true
			}
		}
		if sendAddress != "" {
			msg, err := app.email.constructReset(
				PasswordReset{
					Pin:      pwr.PIN,
					Username: pwr.Username,
					Expiry:   pwr.Expiry,
					Internal: true,
				}, app, false,
			)
			if err != nil {
				app.err.Printf("Failed to construct password reset message for \"%s\": %v", pwr.Username, err)
				respondBool(500, false, gc)
				return
			} else if err := app.sendByID(msg, id); err != nil {
				app.err.Printf("Failed to send password reset message to \"%s\": %v", sendAddress, err)
			} else {
				app.info.Printf("Sent password reset message to \"%s\"", sendAddress)
			}
		}
	}
	if resp.Link != "" && linkCount == 1 {
		gc.JSON(200, resp)
		return
	}
	respondBool(204, true, gc)
}

// @Summary Create a new invite.
// @Produce json
// @Param generateInviteDTO body generateInviteDTO true "New invite request object"
// @Success 200 {object} boolResponse
// @Router /invites [post]
// @Security Bearer
// @tags Invites
func (app *appContext) GenerateInvite(gc *gin.Context) {
	var req generateInviteDTO
	app.debug.Println("Generating new invite")
	app.storage.loadInvites()
	gc.BindJSON(&req)
	currentTime := time.Now()
	validTill := currentTime.AddDate(0, req.Months, req.Days)
	validTill = validTill.Add(time.Hour*time.Duration(req.Hours) + time.Minute*time.Duration(req.Minutes))
	// make sure code doesn't begin with number
	inviteCode := shortuuid.New()
	_, err := strconv.Atoi(string(inviteCode[0]))
	for err == nil {
		inviteCode = shortuuid.New()
		_, err = strconv.Atoi(string(inviteCode[0]))
	}
	var invite Invite
	if req.Label != "" {
		invite.Label = req.Label
	}
	invite.Created = currentTime
	if req.MultipleUses {
		if req.NoLimit {
			invite.NoLimit = true
		} else {
			invite.RemainingUses = req.RemainingUses
		}
	} else {
		invite.RemainingUses = 1
	}
	invite.UserExpiry = req.UserExpiry
	if invite.UserExpiry {
		invite.UserMonths = req.UserMonths
		invite.UserDays = req.UserDays
		invite.UserHours = req.UserHours
		invite.UserMinutes = req.UserMinutes
	}
	invite.ValidTill = validTill
	if req.SendTo != "" && app.config.Section("invite_emails").Key("enabled").MustBool(false) {
		addressValid := false
		discord := ""
		app.debug.Printf("%s: Sending invite message", inviteCode)
		if discordEnabled && !strings.Contains(req.SendTo, "@") {
			users := app.discord.GetUsers(req.SendTo)
			if len(users) == 0 {
				invite.SendTo = fmt.Sprintf("Failed: User not found: \"%s\"", req.SendTo)
			} else if len(users) > 1 {
				invite.SendTo = fmt.Sprintf("Failed: Multiple users found: \"%s\"", req.SendTo)
			} else {
				invite.SendTo = req.SendTo
				addressValid = true
				discord = users[0].User.ID
			}
		} else if emailEnabled {
			addressValid = true
			invite.SendTo = req.SendTo
		}
		if addressValid {
			msg, err := app.email.constructInvite(inviteCode, invite, app, false)
			if err != nil {
				invite.SendTo = fmt.Sprintf("Failed to send to %s", req.SendTo)
				app.err.Printf("%s: Failed to construct invite message: %v", inviteCode, err)
			} else {
				var err error
				if discord != "" {
					err = app.discord.SendDM(msg, discord)
				} else {
					err = app.email.send(msg, req.SendTo)
				}
				if err != nil {
					invite.SendTo = fmt.Sprintf("Failed to send to %s", req.SendTo)
					app.err.Printf("%s: %s: %v", inviteCode, invite.SendTo, err)
				} else {
					app.info.Printf("%s: Sent invite email to \"%s\"", inviteCode, req.SendTo)
				}
			}
		}
	}
	if req.Profile != "" {
		if _, ok := app.storage.profiles[req.Profile]; ok {
			invite.Profile = req.Profile
		} else {
			invite.Profile = "Default"
		}
	}
	app.storage.invites[inviteCode] = invite
	app.storage.storeInvites()
	respondBool(200, true, gc)
}

// @Summary Get invites.
// @Produce json
// @Success 200 {object} getInvitesDTO
// @Router /invites [get]
// @Security Bearer
// @tags Invites
func (app *appContext) GetInvites(gc *gin.Context) {
	app.debug.Println("Invites requested")
	currentTime := time.Now()
	app.storage.loadInvites()
	app.checkInvites()
	var invites []inviteDTO
	for code, inv := range app.storage.invites {
		_, months, days, hours, minutes, _ := timeDiff(inv.ValidTill, currentTime)
		invite := inviteDTO{
			Code:        code,
			Months:      months,
			Days:        days,
			Hours:       hours,
			Minutes:     minutes,
			UserExpiry:  inv.UserExpiry,
			UserMonths:  inv.UserMonths,
			UserDays:    inv.UserDays,
			UserHours:   inv.UserHours,
			UserMinutes: inv.UserMinutes,
			Created:     inv.Created.Unix(),
			Profile:     inv.Profile,
			NoLimit:     inv.NoLimit,
			Label:       inv.Label,
		}
		if len(inv.UsedBy) != 0 {
			invite.UsedBy = map[string]int64{}
			for _, pair := range inv.UsedBy {
				// These used to be stored formatted instead of as a unix timestamp.
				unix, err := strconv.ParseInt(pair[1], 10, 64)
				if err != nil {
					date, err := timefmt.Parse(pair[1], app.datePattern+" "+app.timePattern)
					if err != nil {
						app.err.Printf("Failed to parse usedBy time: %v", err)
					}
					unix = date.Unix()
				}
				invite.UsedBy[pair[0]] = unix
			}
		}
		invite.RemainingUses = 1
		if inv.RemainingUses != 0 {
			invite.RemainingUses = inv.RemainingUses
		}
		if inv.SendTo != "" {
			invite.SendTo = inv.SendTo
		}
		if len(inv.Notify) != 0 {
			var address string
			if app.config.Section("ui").Key("jellyfin_login").MustBool(false) {
				app.storage.loadEmails()
				if addr, ok := app.storage.emails[gc.GetString("jfId")]; ok && addr.Addr != "" {
					address = addr.Addr
				}
			} else {
				address = app.config.Section("ui").Key("email").String()
			}
			if _, ok := inv.Notify[address]; ok {
				if _, ok = inv.Notify[address]["notify-expiry"]; ok {
					invite.NotifyExpiry = inv.Notify[address]["notify-expiry"]
				}
				if _, ok = inv.Notify[address]["notify-creation"]; ok {
					invite.NotifyCreation = inv.Notify[address]["notify-creation"]
				}
			}
		}
		invites = append(invites, invite)
	}
	profiles := make([]string, len(app.storage.profiles))
	if len(app.storage.profiles) != 0 {
		profiles[0] = app.storage.defaultProfile
		i := 1
		if len(app.storage.profiles) > 1 {
			for p := range app.storage.profiles {
				if p != app.storage.defaultProfile {
					profiles[i] = p
					i++
				}
			}
		}
	}
	resp := getInvitesDTO{
		Profiles: profiles,
		Invites:  invites,
	}
	gc.JSON(200, resp)
}

// @Summary Set profile for an invite
// @Produce json
// @Param inviteProfileDTO body inviteProfileDTO true "Invite profile object"
// @Success 200 {object} boolResponse
// @Failure 500 {object} stringResponse
// @Router /invites/profile [post]
// @Security Bearer
// @tags Profiles & Settings
func (app *appContext) SetProfile(gc *gin.Context) {
	var req inviteProfileDTO
	gc.BindJSON(&req)
	app.debug.Printf("%s: Setting profile to \"%s\"", req.Invite, req.Profile)
	// "" means "Don't apply profile"
	if _, ok := app.storage.profiles[req.Profile]; !ok && req.Profile != "" {
		app.err.Printf("%s: Profile \"%s\" not found", req.Invite, req.Profile)
		respond(500, "Profile not found", gc)
		return
	}
	inv := app.storage.invites[req.Invite]
	inv.Profile = req.Profile
	app.storage.invites[req.Invite] = inv
	app.storage.storeInvites()
	respondBool(200, true, gc)
}

// @Summary Get a list of profiles
// @Produce json
// @Success 200 {object} getProfilesDTO
// @Router /profiles [get]
// @Security Bearer
// @tags Profiles & Settings
func (app *appContext) GetProfiles(gc *gin.Context) {
	app.storage.loadProfiles()
	app.debug.Println("Profiles requested")
	out := getProfilesDTO{
		DefaultProfile: app.storage.defaultProfile,
		Profiles:       map[string]profileDTO{},
	}
	for name, p := range app.storage.profiles {
		out.Profiles[name] = profileDTO{
			Admin:         p.Admin,
			LibraryAccess: p.LibraryAccess,
			FromUser:      p.FromUser,
			Ombi:          p.Ombi != nil,
		}
	}
	gc.JSON(200, out)
}

// @Summary Set the default profile to use.
// @Produce json
// @Param profileChangeDTO body profileChangeDTO true "Default profile object"
// @Success 200 {object} boolResponse
// @Failure 500 {object} stringResponse
// @Router /profiles/default [post]
// @Security Bearer
// @tags Profiles & Settings
func (app *appContext) SetDefaultProfile(gc *gin.Context) {
	req := profileChangeDTO{}
	gc.BindJSON(&req)
	app.info.Printf("Setting default profile to \"%s\"", req.Name)
	if _, ok := app.storage.profiles[req.Name]; !ok {
		app.err.Printf("Profile not found: \"%s\"", req.Name)
		respond(500, "Profile not found", gc)
		return
	}
	for name, profile := range app.storage.profiles {
		if name == req.Name {
			profile.Admin = true
			app.storage.profiles[name] = profile
		} else {
			profile.Admin = false
		}
	}
	app.storage.defaultProfile = req.Name
	respondBool(200, true, gc)
}

// @Summary Create a profile based on a Jellyfin user's settings.
// @Produce json
// @Param newProfileDTO body newProfileDTO true "New profile object"
// @Success 200 {object} boolResponse
// @Failure 500 {object} stringResponse
// @Router /profiles [post]
// @Security Bearer
// @tags Profiles & Settings
func (app *appContext) CreateProfile(gc *gin.Context) {
	app.info.Println("Profile creation requested")
	var req newProfileDTO
	gc.BindJSON(&req)
	app.jf.CacheExpiry = time.Now()
	user, status, err := app.jf.UserByID(req.ID, false)
	if !(status == 200 || status == 204) || err != nil {
		app.err.Printf("Failed to get user from Jellyfin (%d): %v", status, err)
		respond(500, "Couldn't get user", gc)
		return
	}
	profile := Profile{
		FromUser: user.Name,
		Policy:   user.Policy,
	}
	app.debug.Printf("Creating profile from user \"%s\"", user.Name)
	if req.Homescreen {
		profile.Configuration = user.Configuration
		profile.Displayprefs, status, err = app.jf.GetDisplayPreferences(req.ID)
		if !(status == 200 || status == 204) || err != nil {
			app.err.Printf("Failed to get DisplayPrefs (%d): %v", status, err)
			respond(500, "Couldn't get displayprefs", gc)
			return
		}
	}
	app.storage.loadProfiles()
	app.storage.profiles[req.Name] = profile
	app.storage.storeProfiles()
	app.storage.loadProfiles()
	respondBool(200, true, gc)
}

// @Summary Delete an existing profile
// @Produce json
// @Param profileChangeDTO body profileChangeDTO true "Delete profile object"
// @Success 200 {object} boolResponse
// @Router /profiles [delete]
// @Security Bearer
// @tags Profiles & Settings
func (app *appContext) DeleteProfile(gc *gin.Context) {
	req := profileChangeDTO{}
	gc.BindJSON(&req)
	name := req.Name
	if _, ok := app.storage.profiles[name]; ok {
		if app.storage.defaultProfile == name {
			app.storage.defaultProfile = ""
		}
		delete(app.storage.profiles, name)
	}
	app.storage.storeProfiles()
	respondBool(200, true, gc)
}

// @Summary Set notification preferences for an invite.
// @Produce json
// @Param setNotifyDTO body setNotifyDTO true "Map of invite codes to notification settings objects"
// @Success 200
// @Failure 400 {object} stringResponse
// @Failure 500 {object} stringResponse
// @Router /invites/notify [post]
// @Security Bearer
// @tags Other
func (app *appContext) SetNotify(gc *gin.Context) {
	var req map[string]map[string]bool
	gc.BindJSON(&req)
	changed := false
	for code, settings := range req {
		app.debug.Printf("%s: Notification settings change requested", code)
		app.storage.loadInvites()
		app.storage.loadEmails()
		invite, ok := app.storage.invites[code]
		if !ok {
			app.err.Printf("%s Notification setting change failed: Invalid code", code)
			respond(400, "Invalid invite code", gc)
			return
		}
		var address string
		jellyfinLogin := app.config.Section("ui").Key("jellyfin_login").MustBool(false)
		if jellyfinLogin {
			var addressAvailable bool = app.getAddressOrName(gc.GetString("jfId")) != ""
			if !addressAvailable {
				app.err.Printf("%s: Couldn't find contact method for admin. Make sure one is set.", code)
				app.debug.Printf("%s: User ID \"%s\"", code, gc.GetString("jfId"))
				respond(500, "Missing user contact method", gc)
				return
			}
			address = gc.GetString("jfId")
		} else {
			address = app.config.Section("ui").Key("email").String()
		}
		if invite.Notify == nil {
			invite.Notify = map[string]map[string]bool{}
		}
		if _, ok := invite.Notify[address]; !ok {
			invite.Notify[address] = map[string]bool{}
		} /*else {
		if _, ok := invite.Notify[address]["notify-expiry"]; !ok {
		*/
		if _, ok := settings["notify-expiry"]; ok && invite.Notify[address]["notify-expiry"] != settings["notify-expiry"] {
			invite.Notify[address]["notify-expiry"] = settings["notify-expiry"]
			app.debug.Printf("%s: Set \"notify-expiry\" to %t for %s", code, settings["notify-expiry"], address)
			changed = true
		}
		if _, ok := settings["notify-creation"]; ok && invite.Notify[address]["notify-creation"] != settings["notify-creation"] {
			invite.Notify[address]["notify-creation"] = settings["notify-creation"]
			app.debug.Printf("%s: Set \"notify-creation\" to %t for %s", code, settings["notify-creation"], address)
			changed = true
		}
		if changed {
			app.storage.invites[code] = invite
		}
	}
	if changed {
		app.storage.storeInvites()
	}
}

// @Summary Delete an invite.
// @Produce json
// @Param deleteInviteDTO body deleteInviteDTO true "Delete invite object"
// @Success 200 {object} boolResponse
// @Failure 400 {object} stringResponse
// @Router /invites [delete]
// @Security Bearer
// @tags Invites
func (app *appContext) DeleteInvite(gc *gin.Context) {
	var req deleteInviteDTO
	gc.BindJSON(&req)
	app.debug.Printf("%s: Deletion requested", req.Code)
	var ok bool
	_, ok = app.storage.invites[req.Code]
	if ok {
		delete(app.storage.invites, req.Code)
		app.storage.storeInvites()
		app.info.Printf("%s: Invite deleted", req.Code)
		respondBool(200, true, gc)
		return
	}
	app.err.Printf("%s: Deletion failed: Invalid code", req.Code)
	respond(400, "Code doesn't exist", gc)
}

// @Summary Get a list of Jellyfin users.
// @Produce json
// @Success 200 {object} getUsersDTO
// @Failure 500 {object} stringResponse
// @Router /users [get]
// @Security Bearer
// @tags Users
func (app *appContext) GetUsers(gc *gin.Context) {
	app.debug.Println("Users requested")
	var resp getUsersDTO
	users, status, err := app.jf.GetUsers(false)
	resp.UserList = make([]respUser, len(users))
	if !(status == 200 || status == 204) || err != nil {
		app.err.Printf("Failed to get users from Jellyfin (%d): %v", status, err)
		respond(500, "Couldn't get users", gc)
		return
	}
	adminOnly := app.config.Section("ui").Key("admin_only").MustBool(true)
	allowAll := app.config.Section("ui").Key("allow_all").MustBool(false)
	i := 0
	app.storage.usersLock.Lock()
	defer app.storage.usersLock.Unlock()
	for _, jfUser := range users {
		user := respUser{
			ID:       jfUser.ID,
			Name:     jfUser.Name,
			Admin:    jfUser.Policy.IsAdministrator,
			Disabled: jfUser.Policy.IsDisabled,
		}
		if !jfUser.LastActivityDate.IsZero() {
			user.LastActive = jfUser.LastActivityDate.Unix()
		}
		if email, ok := app.storage.emails[jfUser.ID]; ok {
			user.Email = email.Addr
			user.NotifyThroughEmail = email.Contact
			user.Label = email.Label
			user.AccountsAdmin = (app.jellyfinLogin) && (email.Admin || (adminOnly && jfUser.Policy.IsAdministrator) || allowAll)
		}
		expiry, ok := app.storage.users[jfUser.ID]
		if ok {
			user.Expiry = expiry.Unix()
		}
		if tgUser, ok := app.storage.telegram[jfUser.ID]; ok {
			user.Telegram = tgUser.Username
			user.NotifyThroughTelegram = tgUser.Contact
		}
		if mxUser, ok := app.storage.matrix[jfUser.ID]; ok {
			user.Matrix = mxUser.UserID
			user.NotifyThroughMatrix = mxUser.Contact
		}
		if dcUser, ok := app.storage.discord[jfUser.ID]; ok {
			user.Discord = dcUser.Username + "#" + dcUser.Discriminator
			user.DiscordID = dcUser.ID
			user.NotifyThroughDiscord = dcUser.Contact
		}
		resp.UserList[i] = user
		i++
	}
	gc.JSON(200, resp)
}

// @Summary Get a list of Ombi users.
// @Produce json
// @Success 200 {object} ombiUsersDTO
// @Failure 500 {object} stringResponse
// @Router /ombi/users [get]
// @Security Bearer
// @tags Ombi
func (app *appContext) OmbiUsers(gc *gin.Context) {
	app.debug.Println("Ombi users requested")
	users, status, err := app.ombi.GetUsers()
	if err != nil || status != 200 {
		app.err.Printf("Failed to get users from Ombi (%d): %v", status, err)
		respond(500, "Couldn't get users", gc)
		return
	}
	userlist := make([]ombiUser, len(users))
	for i, data := range users {
		userlist[i] = ombiUser{
			Name: data["userName"].(string),
			ID:   data["id"].(string),
		}
	}
	gc.JSON(200, ombiUsersDTO{Users: userlist})
}

// @Summary Store Ombi user template in an existing profile.
// @Produce json
// @Param ombiUser body ombiUser true "User to source settings from"
// @Param profile path string true "Name of profile to store in"
// @Success 200 {object} boolResponse
// @Failure 400 {object} boolResponse
// @Failure 500 {object} stringResponse
// @Router /profiles/ombi/{profile} [post]
// @Security Bearer
// @tags Ombi
func (app *appContext) SetOmbiProfile(gc *gin.Context) {
	var req ombiUser
	gc.BindJSON(&req)
	profileName := gc.Param("profile")
	profile, ok := app.storage.profiles[profileName]
	if !ok {
		respondBool(400, false, gc)
		return
	}
	template, code, err := app.ombi.TemplateByID(req.ID)
	if err != nil || code != 200 || len(template) == 0 {
		app.err.Printf("Couldn't get user from Ombi (%d): %v", code, err)
		respond(500, "Couldn't get user", gc)
		return
	}
	profile.Ombi = template
	app.storage.profiles[profileName] = profile
	if err := app.storage.storeProfiles(); err != nil {
		respond(500, "Failed to store profile", gc)
		app.err.Printf("Failed to store profiles: %v", err)
		return
	}
	respondBool(204, true, gc)
}

// @Summary Remove ombi user template from a profile.
// @Produce json
// @Param profile path string true "Name of profile to store in"
// @Success 200 {object} boolResponse
// @Failure 400 {object} boolResponse
// @Failure 500 {object} stringResponse
// @Router /profiles/ombi/{profile} [delete]
// @Security Bearer
// @tags Ombi
func (app *appContext) DeleteOmbiProfile(gc *gin.Context) {
	profileName := gc.Param("profile")
	profile, ok := app.storage.profiles[profileName]
	if !ok {
		respondBool(400, false, gc)
		return
	}
	profile.Ombi = nil
	app.storage.profiles[profileName] = profile
	if err := app.storage.storeProfiles(); err != nil {
		respond(500, "Failed to store profile", gc)
		app.err.Printf("Failed to store profiles: %v", err)
		return
	}
	respondBool(204, true, gc)
}

// @Summary Set whether or not a user can access jfa-go. Redundant if the user is a Jellyfin admin.
// @Produce json
// @Param setAccountsAdminDTO body setAccountsAdminDTO true "Map of userIDs to whether or not they have access."
// @Success 204 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Router /users/accounts-admin [post]
// @Security Bearer
// @tags Users
func (app *appContext) SetAccountsAdmin(gc *gin.Context) {
	var req setAccountsAdminDTO
	gc.BindJSON(&req)
	app.debug.Println("Admin modification requested")
	users, status, err := app.jf.GetUsers(false)
	if !(status == 200 || status == 204) || err != nil {
		app.err.Printf("Failed to get users from Jellyfin (%d): %v", status, err)
		respond(500, "Couldn't get users", gc)
		return
	}
	for _, jfUser := range users {
		id := jfUser.ID
		if admin, ok := req[id]; ok {
			var emailStore = EmailAddress{}
			if oldEmail, ok := app.storage.emails[id]; ok {
				emailStore = oldEmail
			}
			emailStore.Admin = admin
			app.storage.emails[id] = emailStore
		}
	}
	if err := app.storage.storeEmails(); err != nil {
		app.err.Printf("Failed to store email list: %v", err)
		respondBool(500, false, gc)
	}
	app.info.Println("Email list modified")
	respondBool(204, true, gc)
}

// @Summary Modify user's labels, which show next to their name in the accounts tab.
// @Produce json
// @Param modifyEmailsDTO body modifyEmailsDTO true "Map of userIDs to labels"
// @Success 204 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Router /users/labels [post]
// @Security Bearer
// @tags Users
func (app *appContext) ModifyLabels(gc *gin.Context) {
	var req modifyEmailsDTO
	gc.BindJSON(&req)
	app.debug.Println("Label modification requested")
	users, status, err := app.jf.GetUsers(false)
	if !(status == 200 || status == 204) || err != nil {
		app.err.Printf("Failed to get users from Jellyfin (%d): %v", status, err)
		respond(500, "Couldn't get users", gc)
		return
	}
	for _, jfUser := range users {
		id := jfUser.ID
		if label, ok := req[id]; ok {
			var emailStore = EmailAddress{}
			if oldEmail, ok := app.storage.emails[id]; ok {
				emailStore = oldEmail
			}
			emailStore.Label = label
			app.storage.emails[id] = emailStore
		}
	}
	if err := app.storage.storeEmails(); err != nil {
		app.err.Printf("Failed to store email list: %v", err)
		respondBool(500, false, gc)
	}
	app.info.Println("Email list modified")
	respondBool(204, true, gc)
}

// @Summary Modify user's email addresses.
// @Produce json
// @Param modifyEmailsDTO body modifyEmailsDTO true "Map of userIDs to email addresses"
// @Success 200 {object} boolResponse
// @Failure 500 {object} stringResponse
// @Router /users/emails [post]
// @Security Bearer
// @tags Users
func (app *appContext) ModifyEmails(gc *gin.Context) {
	var req modifyEmailsDTO
	gc.BindJSON(&req)
	app.debug.Println("Email modification requested")
	users, status, err := app.jf.GetUsers(false)
	if !(status == 200 || status == 204) || err != nil {
		app.err.Printf("Failed to get users from Jellyfin (%d): %v", status, err)
		respond(500, "Couldn't get users", gc)
		return
	}
	ombiEnabled := app.config.Section("ombi").Key("enabled").MustBool(false)
	for _, jfUser := range users {
		id := jfUser.ID
		if address, ok := req[id]; ok {
			var emailStore = EmailAddress{}
			if oldEmail, ok := app.storage.emails[id]; ok {
				emailStore = oldEmail
			}
			emailStore.Addr = address
			app.storage.emails[id] = emailStore
			if ombiEnabled {
				ombiUser, code, err := app.getOmbiUser(id)
				if code == 200 && err == nil {
					ombiUser["emailAddress"] = address
					code, err = app.ombi.ModifyUser(ombiUser)
					if code != 200 || err != nil {
						app.err.Printf("%s: Failed to change ombi email address (%d): %v", ombiUser["userName"].(string), code, err)
					}
				}
			}
		}
	}
	app.storage.storeEmails()
	app.info.Println("Email list modified")
	respondBool(200, true, gc)
}

// @Summary Resets a user's password with a PIN, and optionally set a new password if given.
// @Produce json
// @Success 200 {object} boolResponse
// @Success 400 {object} PasswordValidation
// @Failure 500 {object} boolResponse
// @Param ResetPasswordDTO body ResetPasswordDTO true "Pin and optional Password."
// @Router /reset [post]
// @tags Other
func (app *appContext) ResetSetPassword(gc *gin.Context) {
	var req ResetPasswordDTO
	gc.BindJSON(&req)
	validation := app.validator.validate(req.Password)
	valid := true
	for _, val := range validation {
		if !val {
			valid = false
		}
	}
	if !valid || req.PIN == "" {
		// 200 bcs idk what i did in js
		app.info.Printf("%s: Password reset failed: Invalid password", req.PIN)
		gc.JSON(400, validation)
		return
	}
	isInternal := false
	var userID, username string
	if reset, ok := app.internalPWRs[req.PIN]; ok {
		isInternal = true
		if time.Now().After(reset.Expiry) {
			app.info.Printf("Password reset failed: PIN \"%s\" has expired", reset.PIN)
			respondBool(401, false, gc)
			delete(app.internalPWRs, req.PIN)
			return
		}
		userID = reset.ID
		username = reset.Username
		status, err := app.jf.ResetPasswordAdmin(userID)
		if !(status == 200 || status == 204) || err != nil {
			app.err.Printf("Password Reset failed (%d): %v", status, err)
			respondBool(status, false, gc)
			return
		}
	} else {
		resp, status, err := app.jf.ResetPassword(req.PIN)
		if status != 200 || err != nil || !resp.Success {
			app.err.Printf("Password Reset failed (%d): %v", status, err)
			respondBool(status, false, gc)
			return
		}
		if req.Password == "" || len(resp.UsersReset) == 0 {
			respondBool(200, false, gc)
			return
		}
		username = resp.UsersReset[0]
	}
	var user mediabrowser.User
	var status int
	var err error
	if isInternal {
		user, status, err = app.jf.UserByID(userID, false)
	} else {
		user, status, err = app.jf.UserByName(username, false)
	}
	if status != 200 || err != nil {
		app.err.Printf("Failed to get user \"%s\" (%d): %v", username, status, err)
		respondBool(500, false, gc)
		return
	}
	prevPassword := req.PIN
	if isInternal {
		prevPassword = ""
	}
	status, err = app.jf.SetPassword(user.ID, prevPassword, req.Password)
	if !(status == 200 || status == 204) || err != nil {
		app.err.Printf("Failed to change password for \"%s\" (%d): %v", username, status, err)
		respondBool(500, false, gc)
		return
	}
	if app.config.Section("ombi").Key("enabled").MustBool(false) {
		// Silently fail for changing ombi passwords
		if status != 200 || err != nil {
			app.err.Printf("Failed to get user \"%s\" from jellyfin/emby (%d): %v", username, status, err)
			respondBool(200, true, gc)
			return
		}
		ombiUser, status, err := app.getOmbiUser(user.ID)
		if status != 200 || err != nil {
			app.err.Printf("Failed to get user \"%s\" from ombi (%d): %v", username, status, err)
			respondBool(200, true, gc)
			return
		}
		ombiUser["password"] = req.Password
		status, err = app.ombi.ModifyUser(ombiUser)
		if status != 200 || err != nil {
			app.err.Printf("Failed to set password for ombi user \"%s\" (%d): %v", ombiUser["userName"], status, err)
			respondBool(200, true, gc)
			return
		}
		app.debug.Printf("Reset password for ombi user \"%s\"", ombiUser["userName"])
	}
	respondBool(200, true, gc)
}

// @Summary Apply settings to a list of users, either from a profile or from another user.
// @Produce json
// @Param userSettingsDTO body userSettingsDTO true "Parameters for applying settings"
// @Success 200 {object} errorListDTO
// @Failure 500 {object} errorListDTO "Lists of errors that occurred while applying settings"
// @Router /users/settings [post]
// @Security Bearer
// @tags Profiles & Settings
func (app *appContext) ApplySettings(gc *gin.Context) {
	app.info.Println("User settings change requested")
	var req userSettingsDTO
	gc.BindJSON(&req)
	applyingFrom := "profile"
	var policy mediabrowser.Policy
	var configuration mediabrowser.Configuration
	var displayprefs map[string]interface{}
	var ombi map[string]interface{}
	if req.From == "profile" {
		app.storage.loadProfiles()
		// Check profile exists & isn't empty
		if _, ok := app.storage.profiles[req.Profile]; !ok || app.storage.profiles[req.Profile].Policy.BlockedTags == nil {
			app.err.Printf("Couldn't find profile \"%s\" or profile was empty", req.Profile)
			respond(500, "Couldn't find profile", gc)
			return
		}
		if req.Homescreen {
			if app.storage.profiles[req.Profile].Configuration.GroupedFolders == nil || len(app.storage.profiles[req.Profile].Displayprefs) == 0 {
				app.err.Printf("No homescreen saved in profile \"%s\"", req.Profile)
				respond(500, "No homescreen template available", gc)
				return
			}
			configuration = app.storage.profiles[req.Profile].Configuration
			displayprefs = app.storage.profiles[req.Profile].Displayprefs
		}
		policy = app.storage.profiles[req.Profile].Policy
		if app.config.Section("ombi").Key("enabled").MustBool(false) {
			profile := app.storage.profiles[req.Profile]
			if profile.Ombi != nil && len(profile.Ombi) != 0 {
				ombi = profile.Ombi
			}
		}

	} else if req.From == "user" {
		applyingFrom = "user"
		app.jf.CacheExpiry = time.Now()
		user, status, err := app.jf.UserByID(req.ID, false)
		if !(status == 200 || status == 204) || err != nil {
			app.err.Printf("Failed to get user from Jellyfin (%d): %v", status, err)
			respond(500, "Couldn't get user", gc)
			return
		}
		applyingFrom = "\"" + user.Name + "\""
		policy = user.Policy
		if req.Homescreen {
			displayprefs, status, err = app.jf.GetDisplayPreferences(req.ID)
			if !(status == 200 || status == 204) || err != nil {
				app.err.Printf("Failed to get DisplayPrefs (%d): %v", status, err)
				respond(500, "Couldn't get displayprefs", gc)
				return
			}
			configuration = user.Configuration
		}
	}
	app.info.Printf("Applying settings to %d user(s) from %s", len(req.ApplyTo), applyingFrom)
	errors := errorListDTO{
		"policy":     map[string]string{},
		"homescreen": map[string]string{},
		"ombi":       map[string]string{},
	}
	/* Jellyfin doesn't seem to like too many of these requests sent in succession
	and can crash and mess up its database. Issue #160 says this occurs when more
	than 100 users are modified. A delay totalling 500ms between requests is used
	if so. */
	var shouldDelay bool = len(req.ApplyTo) >= 100
	if shouldDelay {
		app.debug.Println("Adding delay between requests for large batch")
	}
	for _, id := range req.ApplyTo {
		status, err := app.jf.SetPolicy(id, policy)
		if !(status == 200 || status == 204) || err != nil {
			errors["policy"][id] = fmt.Sprintf("%d: %s", status, err)
		}
		if shouldDelay {
			time.Sleep(250 * time.Millisecond)
		}
		if req.Homescreen {
			status, err = app.jf.SetConfiguration(id, configuration)
			errorString := ""
			if !(status == 200 || status == 204) || err != nil {
				errorString += fmt.Sprintf("Configuration %d: %v ", status, err)
			} else {
				status, err = app.jf.SetDisplayPreferences(id, displayprefs)
				if !(status == 200 || status == 204) || err != nil {
					errorString += fmt.Sprintf("Displayprefs %d: %v ", status, err)
				}
			}
			if errorString != "" {
				errors["homescreen"][id] = errorString
			}
		}
		if ombi != nil {
			errorString := ""
			user, status, err := app.getOmbiUser(id)
			if status != 200 || err != nil {
				errorString += fmt.Sprintf("Ombi GetUser %d: %v ", status, err)
			} else {
				// newUser := ombi
				// newUser["id"] = user["id"]
				// newUser["userName"] = user["userName"]
				// newUser["alias"] = user["alias"]
				// newUser["emailAddress"] = user["emailAddress"]
				for k, v := range ombi {
					switch v.(type) {
					case map[string]interface{}, []interface{}:
						user[k] = v
					default:
						if v != user[k] {
							user[k] = v
						}
					}
				}
				status, err = app.ombi.ModifyUser(user)
				if status != 200 || err != nil {
					errorString += fmt.Sprintf("Apply %d: %v ", status, err)
				}
			}
			if errorString != "" {
				errors["ombi"][id] = errorString
			}
		}
		if shouldDelay {
			time.Sleep(250 * time.Millisecond)
		}
	}
	code := 200
	if len(errors["policy"]) == len(req.ApplyTo) || len(errors["homescreen"]) == len(req.ApplyTo) {
		code = 500
	}
	gc.JSON(code, errors)
}

// @Summary Get jfa-go configuration.
// @Produce json
// @Success 200 {object} settings "Uses the same format as config-base.json"
// @Router /config [get]
// @Security Bearer
// @tags Configuration
func (app *appContext) GetConfig(gc *gin.Context) {
	app.info.Println("Config requested")
	resp := app.configBase
	// Load language options
	formOptions := app.storage.lang.Form.getOptions()
	fl := resp.Sections["ui"].Settings["language-form"]
	fl.Options = formOptions
	fl.Value = app.config.Section("ui").Key("language-form").MustString("en-us")
	pwrOptions := app.storage.lang.PasswordReset.getOptions()
	pl := resp.Sections["password_resets"].Settings["language"]
	pl.Options = pwrOptions
	pl.Value = app.config.Section("password_resets").Key("language").MustString("en-us")
	adminOptions := app.storage.lang.Admin.getOptions()
	al := resp.Sections["ui"].Settings["language-admin"]
	al.Options = adminOptions
	al.Value = app.config.Section("ui").Key("language-admin").MustString("en-us")
	emailOptions := app.storage.lang.Email.getOptions()
	el := resp.Sections["email"].Settings["language"]
	el.Options = emailOptions
	el.Value = app.config.Section("email").Key("language").MustString("en-us")
	telegramOptions := app.storage.lang.Email.getOptions()
	tl := resp.Sections["telegram"].Settings["language"]
	tl.Options = telegramOptions
	tl.Value = app.config.Section("telegram").Key("language").MustString("en-us")
	if updater == "" {
		delete(resp.Sections, "updates")
		for i, v := range resp.Order {
			if v == "updates" {
				resp.Order = append(resp.Order[:i], resp.Order[i+1:]...)
				break
			}
		}
	}
	if PLATFORM == "windows" {
		delete(resp.Sections["smtp"].Settings, "ssl_cert")
		for i, v := range resp.Sections["smtp"].Order {
			if v == "ssl_cert" {
				sect := resp.Sections["smtp"]
				sect.Order = append(sect.Order[:i], sect.Order[i+1:]...)
				resp.Sections["smtp"] = sect
			}
		}
	}
	if !MatrixE2EE() {
		delete(resp.Sections["matrix"].Settings, "encryption")
		for i, v := range resp.Sections["matrix"].Order {
			if v == "encryption" {
				sect := resp.Sections["matrix"]
				sect.Order = append(sect.Order[:i], sect.Order[i+1:]...)
				resp.Sections["matrix"] = sect
			}
		}
	}
	for sectName, section := range resp.Sections {
		for settingName, setting := range section.Settings {
			val := app.config.Section(sectName).Key(settingName)
			s := resp.Sections[sectName].Settings[settingName]
			switch setting.Type {
			case "text", "email", "select", "password":
				s.Value = val.MustString("")
			case "number":
				s.Value = val.MustInt(0)
			case "bool":
				s.Value = val.MustBool(false)
			}
			resp.Sections[sectName].Settings[settingName] = s
		}
	}
	if discordEnabled {
		r, err := app.discord.ListRoles()
		if err == nil {
			roles := make([][2]string, len(r)+1)
			roles[0] = [2]string{"", "None"}
			for i, role := range r {
				roles[i+1] = role
			}
			s := resp.Sections["discord"].Settings["apply_role"]
			s.Options = roles
			resp.Sections["discord"].Settings["apply_role"] = s
		}
	}

	resp.Sections["ui"].Settings["language-form"] = fl
	resp.Sections["ui"].Settings["language-admin"] = al
	resp.Sections["email"].Settings["language"] = el
	resp.Sections["password_resets"].Settings["language"] = pl
	resp.Sections["telegram"].Settings["language"] = tl
	resp.Sections["discord"].Settings["language"] = tl
	resp.Sections["matrix"].Settings["language"] = tl

	// if setting := resp.Sections["invite_emails"].Settings["url_base"]; setting.Value == "" {
	// 	setting.Value = strings.TrimSuffix(resp.Sections["password_resets"].Settings["url_base"].Value.(string), "/invite")
	// 	resp.Sections["invite_emails"].Settings["url_base"] = setting
	// }
	// if setting := resp.Sections["password_resets"].Settings["url_base"]; setting.Value == "" {
	// 	setting.Value = strings.TrimSuffix(resp.Sections["invite_emails"].Settings["url_base"].Value.(string), "/invite")
	// 	resp.Sections["password_resets"].Settings["url_base"] = setting
	// }

	gc.JSON(200, resp)
}

// @Summary Modify app config.
// @Produce json
// @Param appConfig body configDTO true "Config split into sections as in config.ini, all values as strings."
// @Success 200 {object} boolResponse
// @Failure 500 {object} stringResponse
// @Router /config [post]
// @Security Bearer
// @tags Configuration
func (app *appContext) ModifyConfig(gc *gin.Context) {
	app.info.Println("Config modification requested")
	var req configDTO
	gc.BindJSON(&req)
	// Load a new config, as we set various default values in app.config that shouldn't be stored.
	tempConfig, _ := ini.Load(app.configPath)
	for section, settings := range req {
		if section != "restart-program" {
			_, err := tempConfig.GetSection(section)
			if err != nil {
				tempConfig.NewSection(section)
			}
			for setting, value := range settings.(map[string]interface{}) {
				if section == "email" && setting == "method" && value == "disabled" {
					value = ""
				}
				if (section == "discord" || section == "matrix") && setting == "language" {
					tempConfig.Section("telegram").Key("language").SetValue(value.(string))
				} else if value.(string) != app.config.Section(section).Key(setting).MustString("") {
					tempConfig.Section(section).Key(setting).SetValue(value.(string))
				}
			}
		}
	}
	tempConfig.Section("").Key("first_run").SetValue("false")
	if err := tempConfig.SaveTo(app.configPath); err != nil {
		app.err.Printf("Failed to save config to \"%s\": %v", app.configPath, err)
		respond(500, err.Error(), gc)
		return
	}
	app.debug.Println("Config saved")
	gc.JSON(200, map[string]bool{"success": true})
	if req["restart-program"] != nil && req["restart-program"].(bool) {
		app.info.Println("Restarting...")
		if TRAY {
			TRAYRESTART <- true
		} else {
			RESTART <- true
		}
	}
	app.loadConfig()
	// Reinitialize password validator on config change, as opposed to every applicable request like in python.
	if _, ok := req["password_validation"]; ok {
		app.debug.Println("Reinitializing validator")
		validatorConf := ValidatorConf{
			"length":    app.config.Section("password_validation").Key("min_length").MustInt(0),
			"uppercase": app.config.Section("password_validation").Key("upper").MustInt(0),
			"lowercase": app.config.Section("password_validation").Key("lower").MustInt(0),
			"number":    app.config.Section("password_validation").Key("number").MustInt(0),
			"special":   app.config.Section("password_validation").Key("special").MustInt(0),
		}
		if !app.config.Section("password_validation").Key("enabled").MustBool(false) {
			for key := range validatorConf {
				validatorConf[key] = 0
			}
		}
		app.validator.init(validatorConf)
	}
}

// @Summary Get a list of email names and IDs.
// @Produce json
// @Param lang query string false "Language for email titles."
// @Success 200 {object} emailListDTO
// @Router /config/emails [get]
// @Security Bearer
// @tags Configuration
func (app *appContext) GetCustomEmails(gc *gin.Context) {
	lang := gc.Query("lang")
	if _, ok := app.storage.lang.Email[lang]; !ok {
		lang = app.storage.lang.chosenEmailLang
	}
	gc.JSON(200, emailListDTO{
		"UserCreated":       {Name: app.storage.lang.Email[lang].UserCreated["name"], Enabled: app.storage.customEmails.UserCreated.Enabled},
		"InviteExpiry":      {Name: app.storage.lang.Email[lang].InviteExpiry["name"], Enabled: app.storage.customEmails.InviteExpiry.Enabled},
		"PasswordReset":     {Name: app.storage.lang.Email[lang].PasswordReset["name"], Enabled: app.storage.customEmails.PasswordReset.Enabled},
		"UserDeleted":       {Name: app.storage.lang.Email[lang].UserDeleted["name"], Enabled: app.storage.customEmails.UserDeleted.Enabled},
		"UserDisabled":      {Name: app.storage.lang.Email[lang].UserDisabled["name"], Enabled: app.storage.customEmails.UserDisabled.Enabled},
		"UserEnabled":       {Name: app.storage.lang.Email[lang].UserEnabled["name"], Enabled: app.storage.customEmails.UserEnabled.Enabled},
		"InviteEmail":       {Name: app.storage.lang.Email[lang].InviteEmail["name"], Enabled: app.storage.customEmails.InviteEmail.Enabled},
		"WelcomeEmail":      {Name: app.storage.lang.Email[lang].WelcomeEmail["name"], Enabled: app.storage.customEmails.WelcomeEmail.Enabled},
		"EmailConfirmation": {Name: app.storage.lang.Email[lang].EmailConfirmation["name"], Enabled: app.storage.customEmails.EmailConfirmation.Enabled},
		"UserExpired":       {Name: app.storage.lang.Email[lang].UserExpired["name"], Enabled: app.storage.customEmails.UserExpired.Enabled},
	})
}

func (app *appContext) getCustomEmail(id string) *customEmail {
	switch id {
	case "Announcement":
		return &customEmail{}
	case "UserCreated":
		return &app.storage.customEmails.UserCreated
	case "InviteExpiry":
		return &app.storage.customEmails.InviteExpiry
	case "PasswordReset":
		return &app.storage.customEmails.PasswordReset
	case "UserDeleted":
		return &app.storage.customEmails.UserDeleted
	case "UserDisabled":
		return &app.storage.customEmails.UserDisabled
	case "UserEnabled":
		return &app.storage.customEmails.UserEnabled
	case "InviteEmail":
		return &app.storage.customEmails.InviteEmail
	case "WelcomeEmail":
		return &app.storage.customEmails.WelcomeEmail
	case "EmailConfirmation":
		return &app.storage.customEmails.EmailConfirmation
	case "UserExpired":
		return &app.storage.customEmails.UserExpired
	}
	return nil
}

// @Summary Sets the corresponding custom email.
// @Produce json
// @Param customEmail body customEmail true "Content = email (in markdown)."
// @Success 200 {object} boolResponse
// @Failure 400 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Param id path string true "ID of email"
// @Router /config/emails/{id} [post]
// @Security Bearer
// @tags Configuration
func (app *appContext) SetCustomEmail(gc *gin.Context) {
	var req customEmail
	gc.BindJSON(&req)
	id := gc.Param("id")
	if req.Content == "" {
		respondBool(400, false, gc)
		return
	}
	email := app.getCustomEmail(id)
	if email == nil {
		respondBool(400, false, gc)
		return
	}
	email.Content = req.Content
	email.Enabled = true
	if app.storage.storeCustomEmails() != nil {
		respondBool(500, false, gc)
		return
	}
	respondBool(200, true, gc)
}

// @Summary Enable/Disable custom email.
// @Produce json
// @Success 200 {object} boolResponse
// @Failure 400 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Param enable/disable path string true "enable/disable"
// @Param id path string true "ID of email"
// @Router /config/emails/{id}/state/{enable/disable} [post]
// @Security Bearer
// @tags Configuration
func (app *appContext) SetCustomEmailState(gc *gin.Context) {
	id := gc.Param("id")
	s := gc.Param("state")
	enabled := false
	if s == "enable" {
		enabled = true
	} else if s != "disable" {
		respondBool(400, false, gc)
	}
	email := app.getCustomEmail(id)
	if email == nil {
		respondBool(400, false, gc)
		return
	}
	email.Enabled = enabled
	if app.storage.storeCustomEmails() != nil {
		respondBool(500, false, gc)
		return
	}
	respondBool(200, true, gc)
}

// @Summary Returns the custom email (generating it if not set) and list of used variables in it.
// @Produce json
// @Success 200 {object} customEmailDTO
// @Failure 400 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Param id path string true "ID of email"
// @Router /config/emails/{id} [get]
// @Security Bearer
// @tags Configuration
func (app *appContext) GetCustomEmailTemplate(gc *gin.Context) {
	lang := app.storage.lang.chosenEmailLang
	id := gc.Param("id")
	var content string
	var err error
	var msg *Message
	var variables []string
	var conditionals []string
	var values map[string]interface{}
	username := app.storage.lang.Email[lang].Strings.get("username")
	emailAddress := app.storage.lang.Email[lang].Strings.get("emailAddress")
	email := app.getCustomEmail(id)
	if email == nil {
		app.err.Printf("Failed to get custom email with ID \"%s\"", id)
		respondBool(400, false, gc)
		return
	}
	if id == "WelcomeEmail" {
		conditionals = []string{"{yourAccountWillExpire}"}
		email.Conditionals = conditionals
	}
	content = email.Content
	noContent := content == ""
	if !noContent {
		variables = email.Variables
	}
	switch id {
	case "Announcement":
		// Just send the email html
		content = ""
	case "UserCreated":
		if noContent {
			msg, err = app.email.constructCreated("", "", "", Invite{}, app, true)
		}
		values = app.email.createdValues("xxxxxx", username, emailAddress, Invite{}, app, false)
	case "InviteExpiry":
		if noContent {
			msg, err = app.email.constructExpiry("", Invite{}, app, true)
		}
		values = app.email.expiryValues("xxxxxx", Invite{}, app, false)
	case "PasswordReset":
		if noContent {
			msg, err = app.email.constructReset(PasswordReset{}, app, true)
		}
		values = app.email.resetValues(PasswordReset{Pin: "12-34-56", Username: username}, app, false)
	case "UserDeleted":
		if noContent {
			msg, err = app.email.constructDeleted("", app, true)
		}
		values = app.email.deletedValues(app.storage.lang.Email[lang].Strings.get("reason"), app, false)
	case "UserDisabled":
		if noContent {
			msg, err = app.email.constructDisabled("", app, true)
		}
		values = app.email.deletedValues(app.storage.lang.Email[lang].Strings.get("reason"), app, false)
	case "UserEnabled":
		if noContent {
			msg, err = app.email.constructEnabled("", app, true)
		}
		values = app.email.deletedValues(app.storage.lang.Email[lang].Strings.get("reason"), app, false)
	case "InviteEmail":
		if noContent {
			msg, err = app.email.constructInvite("", Invite{}, app, true)
		}
		values = app.email.inviteValues("xxxxxx", Invite{}, app, false)
	case "WelcomeEmail":
		if noContent {
			msg, err = app.email.constructWelcome("", time.Time{}, app, true)
		}
		values = app.email.welcomeValues(username, time.Now(), app, false, true)
	case "EmailConfirmation":
		if noContent {
			msg, err = app.email.constructConfirmation("", "", "", app, true)
		}
		values = app.email.confirmationValues("xxxxxx", username, "xxxxxx", app, false)
	case "UserExpired":
		if noContent {
			msg, err = app.email.constructUserExpired(app, true)
		}
		values = app.email.userExpiredValues(app, false)
	}
	if err != nil {
		respondBool(500, false, gc)
		return
	}
	if noContent && id != "Announcement" {
		content = msg.Text
		variables = make([]string, strings.Count(content, "{"))
		i := 0
		found := false
		buf := ""
		for _, c := range content {
			if !found && c != '{' && c != '}' {
				continue
			}
			found = true
			buf += string(c)
			if c == '}' {
				found = false
				variables[i] = buf
				buf = ""
				i++
			}
		}
		email.Variables = variables
	}
	if variables == nil {
		variables = []string{}
	}
	if app.storage.storeCustomEmails() != nil {
		respondBool(500, false, gc)
		return
	}
	mail, err := app.email.constructTemplate("", "<div class=\"preview-content\"></div>", app)
	if err != nil {
		respondBool(500, false, gc)
		return
	}
	gc.JSON(200, customEmailDTO{Content: content, Variables: variables, Conditionals: conditionals, Values: values, HTML: mail.HTML, Plaintext: mail.Text})
}

// @Summary Returns whether there's a new update, and extra info if there is.
// @Produce json
// @Success 200 {object} checkUpdateDTO
// @Router /config/update [get]
// @Security Bearer
// @tags Configuration
func (app *appContext) CheckUpdate(gc *gin.Context) {
	if !app.newUpdate {
		app.update = Update{}
	}
	gc.JSON(200, checkUpdateDTO{New: app.newUpdate, Update: app.update})
}

// @Summary Apply an update.
// @Produce json
// @Success 200 {object} boolResponse
// @Success 400 {object} stringResponse
// @Success 500 {object} boolResponse
// @Router /config/update [post]
// @Security Bearer
// @tags Configuration
func (app *appContext) ApplyUpdate(gc *gin.Context) {
	if !app.update.CanUpdate {
		respond(400, "Update is manual", gc)
		return
	}
	err := app.update.update()
	if err != nil {
		app.err.Printf("Failed to apply update: %v", err)
		respondBool(500, false, gc)
		return
	}
	if PLATFORM == "windows" {
		respondBool(500, true, gc)
		return
	}
	respondBool(200, true, gc)
	app.HardRestart()
}

// @Summary Logout by deleting refresh token from cookies.
// @Produce json
// @Success 200 {object} boolResponse
// @Failure 500 {object} stringResponse
// @Router /logout [post]
// @Security Bearer
// @tags Other
func (app *appContext) Logout(gc *gin.Context) {
	cookie, err := gc.Cookie("refresh")
	if err != nil {
		app.debug.Printf("Couldn't get cookies: %s", err)
		respond(500, "Couldn't fetch cookies", gc)
		return
	}
	app.invalidTokens = append(app.invalidTokens, cookie)
	gc.SetCookie("refresh", "invalid", -1, "/", gc.Request.URL.Hostname(), true, true)
	respondBool(200, true, gc)
}

// @Summary Returns a map of available language codes to their full names, usable in the lang query parameter.
// @Produce json
// @Success 200 {object} langDTO
// @Failure 500 {object} stringResponse
// @Param page path string true "admin/form/setup/email/pwr"
// @Router /lang/{page} [get]
// @tags Other
func (app *appContext) GetLanguages(gc *gin.Context) {
	page := gc.Param("page")
	resp := langDTO{}
	switch page {
	case "form":
		for key, lang := range app.storage.lang.Form {
			resp[key] = lang.Meta.Name
		}
	case "admin":
		for key, lang := range app.storage.lang.Admin {
			resp[key] = lang.Meta.Name
		}
	case "setup":
		for key, lang := range app.storage.lang.Setup {
			resp[key] = lang.Meta.Name
		}
	case "email":
		for key, lang := range app.storage.lang.Email {
			resp[key] = lang.Meta.Name
		}
	case "pwr":
		for key, lang := range app.storage.lang.PasswordReset {
			resp[key] = lang.Meta.Name
		}
	}
	if len(resp) == 0 {
		respond(500, "Couldn't get languages", gc)
		return
	}
	gc.JSON(200, resp)
}

// @Summary Serves a translations for pages "admin" or "form".
// @Produce json
// @Success 200 {object} adminLang
// @Failure 400 {object} boolResponse
// @Param page path string true "admin or form."
// @Param language path string true "language code, e.g en-us."
// @Router /lang/{page}/{language} [get]
// @tags Other
func (app *appContext) ServeLang(gc *gin.Context) {
	page := gc.Param("page")
	lang := strings.Replace(gc.Param("file"), ".json", "", 1)
	if page == "admin" {
		gc.JSON(200, app.storage.lang.Admin[lang])
		return
	} else if page == "form" {
		gc.JSON(200, app.storage.lang.Form[lang])
		return
	}
	respondBool(400, false, gc)
}

// @Summary Returns a new Telegram verification PIN, and the bot username.
// @Produce json
// @Success 200 {object} telegramPinDTO
// @Router /telegram/pin [get]
// @Security Bearer
// @tags Other
func (app *appContext) TelegramGetPin(gc *gin.Context) {
	gc.JSON(200, telegramPinDTO{
		Token:    app.telegram.NewAuthToken(),
		Username: app.telegram.username,
	})
}

// @Summary Link a Jellyfin & Telegram user together via a verification PIN.
// @Produce json
// @Param telegramSetDTO body telegramSetDTO true "Token and user's Jellyfin ID."
// @Success 200 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Failure 400 {object} boolResponse
// @Router /users/telegram [post]
// @Security Bearer
// @tags Other
func (app *appContext) TelegramAddUser(gc *gin.Context) {
	var req telegramSetDTO
	gc.BindJSON(&req)
	if req.Token == "" || req.ID == "" {
		respondBool(400, false, gc)
		return
	}
	tokenIndex := -1
	for i, v := range app.telegram.verifiedTokens {
		if v.Token == req.Token {
			tokenIndex = i
			break
		}
	}
	if tokenIndex == -1 {
		respondBool(500, false, gc)
		return
	}
	tgToken := app.telegram.verifiedTokens[tokenIndex]
	tgUser := TelegramUser{
		ChatID:   tgToken.ChatID,
		Username: tgToken.Username,
		Contact:  true,
	}
	if lang, ok := app.telegram.languages[tgToken.ChatID]; ok {
		tgUser.Lang = lang
	}
	if app.storage.telegram == nil {
		app.storage.telegram = map[string]TelegramUser{}
	}
	app.storage.telegram[req.ID] = tgUser
	err := app.storage.storeTelegramUsers()
	if err != nil {
		app.err.Printf("Failed to store Telegram users: %v", err)
	} else {
		app.telegram.verifiedTokens[len(app.telegram.verifiedTokens)-1], app.telegram.verifiedTokens[tokenIndex] = app.telegram.verifiedTokens[tokenIndex], app.telegram.verifiedTokens[len(app.telegram.verifiedTokens)-1]
		app.telegram.verifiedTokens = app.telegram.verifiedTokens[:len(app.telegram.verifiedTokens)-1]
	}
	linkExistingOmbiDiscordTelegram(app)
	respondBool(200, true, gc)
}

// @Summary Sets whether to notify a user through telegram/discord/matrix/email or not.
// @Produce json
// @Param SetContactMethodsDTO body SetContactMethodsDTO true "User's Jellyfin ID and whether or not to notify then through Telegram."
// @Success 200 {object} boolResponse
// @Success 400 {object} boolResponse
// @Success 500 {object} boolResponse
// @Router /users/telegram/notify [post]
// @Security Bearer
// @tags Other
func (app *appContext) SetContactMethods(gc *gin.Context) {
	var req SetContactMethodsDTO
	gc.BindJSON(&req)
	if req.ID == "" {
		respondBool(400, false, gc)
		return
	}
	if tgUser, ok := app.storage.telegram[req.ID]; ok {
		change := tgUser.Contact != req.Telegram
		tgUser.Contact = req.Telegram
		app.storage.telegram[req.ID] = tgUser
		if err := app.storage.storeTelegramUsers(); err != nil {
			respondBool(500, false, gc)
			app.err.Printf("Telegram: Failed to store users: %v", err)
			return
		}
		if change {
			msg := ""
			if !req.Telegram {
				msg = " not"
			}
			app.debug.Printf("Telegram: User \"%s\" will%s be notified through Telegram.", tgUser.Username, msg)
		}
	}
	if dcUser, ok := app.storage.discord[req.ID]; ok {
		change := dcUser.Contact != req.Discord
		dcUser.Contact = req.Discord
		app.storage.discord[req.ID] = dcUser
		if err := app.storage.storeDiscordUsers(); err != nil {
			respondBool(500, false, gc)
			app.err.Printf("Discord: Failed to store users: %v", err)
			return
		}
		if change {
			msg := ""
			if !req.Discord {
				msg = " not"
			}
			app.debug.Printf("Discord: User \"%s\" will%s be notified through Discord.", dcUser.Username, msg)
		}
	}
	if mxUser, ok := app.storage.matrix[req.ID]; ok {
		change := mxUser.Contact != req.Matrix
		mxUser.Contact = req.Matrix
		app.storage.matrix[req.ID] = mxUser
		if err := app.storage.storeMatrixUsers(); err != nil {
			respondBool(500, false, gc)
			app.err.Printf("Matrix: Failed to store users: %v", err)
			return
		}
		if change {
			msg := ""
			if !req.Matrix {
				msg = " not"
			}
			app.debug.Printf("Matrix: User \"%s\" will%s be notified through Matrix.", mxUser.UserID, msg)
		}
	}
	if email, ok := app.storage.emails[req.ID]; ok {
		change := email.Contact != req.Email
		email.Contact = req.Email
		app.storage.emails[req.ID] = email
		if err := app.storage.storeEmails(); err != nil {
			respondBool(500, false, gc)
			app.err.Printf("Failed to store emails: %v", err)
			return
		}
		if change {
			msg := ""
			if !req.Email {
				msg = " not"
			}
			app.debug.Printf("\"%s\" will%s be notified via Email.", email.Addr, msg)
		}
	}
	respondBool(200, true, gc)
}

// @Summary Returns true/false on whether or not a telegram PIN was verified. Requires bearer auth.
// @Produce json
// @Success 200 {object} boolResponse
// @Param pin path string true "PIN code to check"
// @Router /telegram/verified/{pin} [get]
// @Security Bearer
// @tags Other
func (app *appContext) TelegramVerified(gc *gin.Context) {
	pin := gc.Param("pin")
	tokenIndex := -1
	for i, v := range app.telegram.verifiedTokens {
		if v.Token == pin {
			tokenIndex = i
			break
		}
	}
	// if tokenIndex != -1 {
	// 	length := len(app.telegram.verifiedTokens)
	// 	app.telegram.verifiedTokens[length-1], app.telegram.verifiedTokens[tokenIndex] = app.telegram.verifiedTokens[tokenIndex], app.telegram.verifiedTokens[length-1]
	// 	app.telegram.verifiedTokens = app.telegram.verifiedTokens[:length-1]
	// }
	respondBool(200, tokenIndex != -1, gc)
}

// @Summary Returns true/false on whether or not a telegram PIN was verified. Requires invite code.
// @Produce json
// @Success 200 {object} boolResponse
// @Success 401 {object} boolResponse
// @Param pin path string true "PIN code to check"
// @Param invCode path string true "invite Code"
// @Router /invite/{invCode}/telegram/verified/{pin} [get]
// @tags Other
func (app *appContext) TelegramVerifiedInvite(gc *gin.Context) {
	code := gc.Param("invCode")
	if _, ok := app.storage.invites[code]; !ok {
		respondBool(401, false, gc)
		return
	}
	pin := gc.Param("pin")
	tokenIndex := -1
	for i, v := range app.telegram.verifiedTokens {
		if v.Token == pin {
			tokenIndex = i
			break
		}
	}
	// if tokenIndex != -1 {
	// 	length := len(app.telegram.verifiedTokens)
	// 	app.telegram.verifiedTokens[length-1], app.telegram.verifiedTokens[tokenIndex] = app.telegram.verifiedTokens[tokenIndex], app.telegram.verifiedTokens[length-1]
	// 	app.telegram.verifiedTokens = app.telegram.verifiedTokens[:length-1]
	// }
	respondBool(200, tokenIndex != -1, gc)
}

// @Summary Returns true/false on whether or not a discord PIN was verified. Requires invite code.
// @Produce json
// @Success 200 {object} boolResponse
// @Failure 401 {object} boolResponse
// @Param pin path string true "PIN code to check"
// @Param invCode path string true "invite Code"
// @Router /invite/{invCode}/discord/verified/{pin} [get]
// @tags Other
func (app *appContext) DiscordVerifiedInvite(gc *gin.Context) {
	code := gc.Param("invCode")
	if _, ok := app.storage.invites[code]; !ok {
		respondBool(401, false, gc)
		return
	}
	pin := gc.Param("pin")
	_, ok := app.discord.verifiedTokens[pin]
	respondBool(200, ok, gc)
}

// @Summary Returns a 10-minute, one-use Discord server invite
// @Produce json
// @Success 200 {object} DiscordInviteDTO
// @Failure 400 {object} boolResponse
// @Failure 401 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Param invCode path string true "invite Code"
// @Router /invite/{invCode}/discord/invite [get]
// @tags Other
func (app *appContext) DiscordServerInvite(gc *gin.Context) {
	if app.discord.inviteChannelName == "" {
		respondBool(400, false, gc)
		return
	}
	code := gc.Param("invCode")
	if _, ok := app.storage.invites[code]; !ok {
		respondBool(401, false, gc)
		return
	}
	invURL, iconURL := app.discord.NewTempInvite(10*60, 1)
	if invURL == "" {
		respondBool(500, false, gc)
		return
	}
	gc.JSON(200, DiscordInviteDTO{invURL, iconURL})
}

// @Summary Generate and send a new PIN to a specified Matrix user.
// @Produce json
// @Success 200 {object} boolResponse
// @Failure 400 {object} boolResponse
// @Failure 401 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Param invCode path string true "invite Code"
// @Param MatrixSendPINDTO body MatrixSendPINDTO true "User's Matrix ID."
// @Router /invite/{invCode}/matrix/user [post]
// @tags Other
func (app *appContext) MatrixSendPIN(gc *gin.Context) {
	code := gc.Param("invCode")
	if _, ok := app.storage.invites[code]; !ok {
		respondBool(401, false, gc)
		return
	}
	var req MatrixSendPINDTO
	gc.BindJSON(&req)
	if req.UserID == "" {
		respondBool(400, false, gc)
		return
	}
	ok := app.matrix.SendStart(req.UserID)
	if !ok {
		respondBool(500, false, gc)
		return
	}
	respondBool(200, true, gc)
}

// @Summary Check whether a matrix PIN is valid, and mark the token as verified if so. Requires invite code.
// @Produce json
// @Success 200 {object} boolResponse
// @Failure 401 {object} boolResponse
// @Param pin path string true "PIN code to check"
// @Param invCode path string true "invite Code"
// @Param userID path string true "Matrix User ID"
// @Router /invite/{invCode}/matrix/verified/{userID}/{pin} [get]
// @tags Other
func (app *appContext) MatrixCheckPIN(gc *gin.Context) {
	code := gc.Param("invCode")
	if _, ok := app.storage.invites[code]; !ok {
		app.debug.Println("Matrix: Invite code was invalid")
		respondBool(401, false, gc)
		return
	}
	userID := gc.Param("userID")
	pin := gc.Param("pin")
	user, ok := app.matrix.tokens[pin]
	if !ok {
		app.debug.Println("Matrix: PIN not found")
		respondBool(200, false, gc)
		return
	}
	if user.User.UserID != userID {
		app.debug.Println("Matrix: User ID of PIN didn't match")
		respondBool(200, false, gc)
		return
	}
	user.Verified = true
	app.matrix.tokens[pin] = user
	respondBool(200, true, gc)
}

// @Summary Generates a Matrix access token from a username and password.
// @Produce json
// @Success 200 {object} boolResponse
// @Failure 400 {object} stringResponse
// @Failure 401 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Param MatrixLoginDTO body MatrixLoginDTO true "Username & password."
// @Router /matrix/login [post]
// @tags Other
func (app *appContext) MatrixLogin(gc *gin.Context) {
	var req MatrixLoginDTO
	gc.BindJSON(&req)
	if req.Username == "" || req.Password == "" {
		respond(400, "errorLoginBlank", gc)
		return
	}
	token, err := app.matrix.generateAccessToken(req.Homeserver, req.Username, req.Password)
	if err != nil {
		app.err.Printf("Matrix: Failed to generate token: %v", err)
		respond(401, "Unauthorized", gc)
		return
	}
	tempConfig, _ := ini.Load(app.configPath)
	matrix := tempConfig.Section("matrix")
	matrix.Key("enabled").SetValue("true")
	matrix.Key("homeserver").SetValue(req.Homeserver)
	matrix.Key("token").SetValue(token)
	matrix.Key("user_id").SetValue(req.Username)
	if err := tempConfig.SaveTo(app.configPath); err != nil {
		app.err.Printf("Failed to save config to \"%s\": %v", app.configPath, err)
		respondBool(500, false, gc)
		return
	}
	respondBool(200, true, gc)
}

// @Summary Links a Matrix user to a Jellyfin account via user IDs. Notifications are turned on by default.
// @Produce json
// @Success 200 {object} boolResponse
// @Failure 400 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Param MatrixConnectUserDTO body MatrixConnectUserDTO true "User's Jellyfin ID & Matrix user ID."
// @Router /users/matrix [post]
// @tags Other
func (app *appContext) MatrixConnect(gc *gin.Context) {
	var req MatrixConnectUserDTO
	gc.BindJSON(&req)
	if app.storage.matrix == nil {
		app.storage.matrix = map[string]MatrixUser{}
	}
	roomID, encrypted, err := app.matrix.CreateRoom(req.UserID)
	if err != nil {
		app.err.Printf("Matrix: Failed to create room: %v", err)
		respondBool(500, false, gc)
		return
	}
	app.storage.matrix[req.JellyfinID] = MatrixUser{
		UserID:    req.UserID,
		RoomID:    string(roomID),
		Lang:      "en-us",
		Contact:   true,
		Encrypted: encrypted,
	}
	app.matrix.isEncrypted[roomID] = encrypted
	if err := app.storage.storeMatrixUsers(); err != nil {
		app.err.Printf("Failed to store Matrix users: %v", err)
		respondBool(500, false, gc)
		return
	}
	respondBool(200, true, gc)
}

// @Summary Returns a list of matching users from a Discord guild, given a username (discriminator optional).
// @Produce json
// @Success 200 {object} DiscordUsersDTO
// @Failure 400 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Param username path string true "username to search."
// @Router /users/discord/{username} [get]
// @tags Other
func (app *appContext) DiscordGetUsers(gc *gin.Context) {
	name := gc.Param("username")
	if name == "" {
		respondBool(400, false, gc)
		return
	}
	users := app.discord.GetUsers(name)
	resp := DiscordUsersDTO{Users: make([]DiscordUserDTO, len(users))}
	for i, u := range users {
		resp.Users[i] = DiscordUserDTO{
			Name:      u.User.Username + "#" + u.User.Discriminator,
			ID:        u.User.ID,
			AvatarURL: u.User.AvatarURL("32"),
		}
	}
	gc.JSON(200, resp)
}

// @Summary Links a Discord account to a Jellyfin account via user IDs. Notifications are turned on by default.
// @Produce json
// @Success 200 {object} boolResponse
// @Failure 400 {object} boolResponse
// @Failure 500 {object} boolResponse
// @Param DiscordConnectUserDTO body DiscordConnectUserDTO true "User's Jellyfin ID & Discord ID."
// @Router /users/discord [post]
// @tags Other
func (app *appContext) DiscordConnect(gc *gin.Context) {
	var req DiscordConnectUserDTO
	gc.BindJSON(&req)
	if req.JellyfinID == "" || req.DiscordID == "" {
		respondBool(400, false, gc)
		return
	}
	user, ok := app.discord.NewUser(req.DiscordID)
	if !ok {
		respondBool(500, false, gc)
		return
	}
	app.storage.discord[req.JellyfinID] = user
	if err := app.storage.storeDiscordUsers(); err != nil {
		app.err.Printf("Failed to store Discord users: %v", err)
		respondBool(500, false, gc)
		return
	}
	linkExistingOmbiDiscordTelegram(app)
	respondBool(200, true, gc)
}

// @Summary Restarts the program. No response means success.
// @Router /restart [post]
// @Security Bearer
// @tags Other
func (app *appContext) restart(gc *gin.Context) {
	app.info.Println("Restarting...")
	err := app.Restart()
	if err != nil {
		app.err.Printf("Couldn't restart, try restarting manually: %v", err)
	}
}

// @Summary Returns the last 100 lines of the log.
// @Router /log [get]
// @Success 200 {object} LogDTO
// @Security Bearer
// @tags Other
func (app *appContext) GetLog(gc *gin.Context) {
	gc.JSON(200, LogDTO{lineCache.String()})
}

// no need to syscall.exec anymore!
func (app *appContext) Restart() error {
	if TRAY {
		TRAYRESTART <- true
	} else {
		RESTART <- true
	}
	return nil
}
