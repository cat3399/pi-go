package app

import (
	"testing"
	"unicode/utf8"
)

func FuzzProductionConfigAdmissionNeverPanics(f *testing.F) {
	for _, seed := range []string{
		`{"providers":{"openai":{"baseUrl":"https://api.openai.com/v1","apiKey":"$OPENAI_API_KEY"}}}`,
		"// comment\n{\"providers\": {\"openai\": {\"apiKey\": \"$$literal\",},},}",
		`{"openai":{"type":"api_key","key":"${SCOPED_KEY}","env":{"SCOPED_KEY":"fixture"}}}`,
		`{"duplicate":1,"duplicate":2}`,
		"",
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, input []byte) {
		if !utf8.Valid(input) {
			return
		}
		normalized := normalizeJSONWithLineComments(input)
		_, _ = decodeStrictJSON(normalized)
		_, _ = resolveProductionConfigValue(
			string(input),
			"fuzz value",
			map[string]string{"SCOPED_KEY": "scoped"},
			map[string]string{"OPENAI_API_KEY": "ambient"},
		)
	})
}
