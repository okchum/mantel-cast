package cast

import (
	"strings"
	"testing"
)

const eureka = `{"name":"Bedroom Display","timezone":"Pacific/Auckland","night_mode_params":{"device_override_do_not_disturb":0,"do_not_disturb":false,"enabled":true,"led_brightness":1.0,"volume":1.0,"windows":[{"days":[0,1,2,3,4,5,6],"length_hours":8.0,"start_hour":22.0}]}}`

func TestParseNightMode(t *testing.T) {
	n, err := parseNightMode(strings.NewReader(eureka))
	if err != nil {
		t.Fatal(err)
	}
	if !n.Enabled || n.DoNotDisturb || n.Timezone != "Pacific/Auckland" || len(n.Windows) != 1 || n.Windows[0].String() != "22:00+8h" {
		t.Fatalf("%+v", n)
	}
	if _, err := parseNightMode(strings.NewReader(`{"name":"x"}`)); err == nil {
		t.Fatal("missing params should fail")
	}
}
