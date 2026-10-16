package main

import "testing"

func TestCalculator(t *testing.T) {
	cases := map[string]string{
		"17 * 23":              "391",
		"17 x 23":              "391",
		"0.2 * 85":             "17",
		"(2 + 3) * 4 - 1":      "19",
		"2 ^ 3 ^ 2":            "512",
		"-3 + 5":               "2",
		"10 / 4":               "2.5",
		"17 % 5":               "2",
		"sqrt(144) + 1":        "13",
		"1,250 * 2":            "2500",
		"round(2.5) + abs(-4)": "7",
	}
	for in, want := range cases {
		v, err := evalExpression(in)
		if err != nil || formatNumber(v) != want {
			t.Errorf("%q = %v (%v), want %s", in, formatNumber(v), err, want)
		}
	}
	for _, bad := range []string{"", "1 +", "2 / 0", "foo(3)", "sqrt(-1)", "(1", "1 2", "abc"} {
		if _, err := evalExpression(bad); err == nil {
			t.Errorf("%q should be an error", bad)
		}
	}
}
