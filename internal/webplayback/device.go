package webplayback

import (
	_ "embed"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"

	widevine "github.com/iyear/gowidevine"
)

// The bundled Widevine L3 device — the same public "AOSP on IA Emulator"
// client-id blob + RSA key that apple-music-downloader ships in
// utils/runv3/cdm. It is not a secret and not tied to any account; Apple's
// acquireWebPlaybackLicense accepts it for the AAC-256 (`28:ctrp256`) flavor.
// If Apple ever revokes it, licensing here starts failing and this file is
// what needs a refreshed device.
//
//go:embed l3_device.pem
var l3PrivateKeyPEM []byte

//go:embed l3_device.clientid
var l3ClientIDBase64 string

var (
	deviceOnce sync.Once
	deviceVal  *widevine.Device
	deviceErr  error
)

// device loads the bundled L3 device once and memoises it.
func device() (*widevine.Device, error) {
	deviceOnce.Do(func() {
		clientID, err := base64.StdEncoding.DecodeString(strings.TrimSpace(l3ClientIDBase64))
		if err != nil {
			deviceErr = fmt.Errorf("decode bundled client id: %w", err)
			return
		}
		deviceVal, deviceErr = widevine.NewDevice(widevine.FromRaw(clientID, l3PrivateKeyPEM))
		if deviceErr != nil {
			deviceErr = fmt.Errorf("load bundled widevine device: %w", deviceErr)
		}
	})
	return deviceVal, deviceErr
}
