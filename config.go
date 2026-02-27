package main

var Config = struct {
	OwnerNumber   string
	BotName       string
	OTPChannelIDs []string
	OTPApiURLs    []string
	Interval      int
}{
	OwnerNumber: "923556692797",
	BotName:     "Kami OTP Monitor",
	OTPChannelIDs: []string{
		"120363407230990898@newsletter",
	},
	OTPApiURLs: []string{
		"http://kami-api-production-40eb.up.railway.app/api/np?type=sms",
		"http://kami-api-production-40eb.up.railway.app/api/np1?type=sms",
		"https://kami-api-production.up.railway.app/api/roxy?type=sms",
		"https://kami-api-production.up.railway.app/api/msi?type=sms",
		"https://kami-api-production.up.railway.app/api/goat?type=sms",
	},
	Interval: 5,
}
