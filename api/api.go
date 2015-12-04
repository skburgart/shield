package api

import (
	"fmt"
	"os"
)

type YesNo struct {
	On  bool
	Yes bool
}

func MaybeString(tf string) YesNo {
	if tf == "" {
		return YesNo{}
	}
	return Maybe(tf == "t")
}

func Maybe(tf bool) YesNo {
	return YesNo{On: true, Yes: tf}
}

func No() YesNo {
	return Maybe(false)
}

func Yes() YesNo {
	return Maybe(true)
}

func (yn *YesNo) Given() bool {
	if yn == nil {
		return false
	}
	return yn.On
}

func ShieldURI(p string, args ...interface{}) *URL {
	endpoint := os.Getenv("SHIELD_TARGET")
	if endpoint == "" {
		endpoint = "https://shield"
	}

	path := fmt.Sprintf(p, args...)
	u, err := ParseURL(fmt.Sprintf("%s%s", endpoint, path))
	if err != nil {
		panic(err)
	}
	return u
}
