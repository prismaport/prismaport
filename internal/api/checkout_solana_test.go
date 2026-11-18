package api

import "testing"

func TestSentence(t *testing.T) {
	for in, want := range map[string]string{
		"no such session": "No such session.",
		"this merchant cannot receive USDC from a connected wallet yet; send it to the deposit address instead": "This merchant cannot receive USDC from a connected wallet yet; send it to the deposit address instead.",
		"Already a sentence.": "Already a sentence.",
		"  padded  ":          "Padded.",
		"":                    "The payment request failed.",
		"énorme":              "Énorme.",
		"is it?":              "Is it?",
	} {
		t.Run(in, func(t *testing.T) {
			if got := sentence(in); got != want {
				t.Errorf("sentence(%q) = %q, want %q", in, got, want)
			}
		})
	}
}
