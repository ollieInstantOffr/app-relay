package model

import "strings"

// Validate implements Validator for Settings → Updates (engine slice).
func (s *EnginesSettings) Validate() error {
	e := Errs{}
	switch s.NginxChannel {
	case "stable", "mainline":
	default:
		e.Add("nginxChannel", "Pick stable or mainline")
	}
	switch s.HAProxyChannel {
	case "lts", "latest":
	default:
		e.Add("haproxyChannel", "Pick lts or latest")
	}
	if s.CheckIntervalHours < 1 || s.CheckIntervalHours > 24*14 {
		e.Add("checkIntervalHours", "Between 1 hour and 14 days")
	}
	for field, img := range map[string]string{"nginxImage": s.NginxImage, "haproxyImage": s.HAProxyImage} {
		if strings.ContainsAny(img, " \t\n") {
			e.Add(field, "Not a valid image reference")
		}
	}
	return e.Err()
}
