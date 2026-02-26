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
		"120363423562861659@newsletter",
		"120363407230990898@newsletter",
	},
	OTPApiURLs: []string{
		"https://api-kami-nodejs-production-a53d.up.railway.app/api/sms",
		"https://api-node-js-new-production-b09a.up.railway.app/api?type=sms",
		"https://kami-api-production.up.railway.app/api/roxy?type=sms",
		"https://kami-api-production.up.railway.app/api/msi?type=sms",
		"https://kami-api-production.up.railway.app/api/goat?type=sms",
	},
	Interval: 5,
}
