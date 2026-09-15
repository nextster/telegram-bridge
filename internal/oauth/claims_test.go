package oauth

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClaimShapeOmitsValues(t *testing.T) {
	var claims map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"sub":"secret-sub","name":"John Doe","iat":1700000000,"verified":true,"extra":{"a":1},"list":[1],"none":null}`), &claims); err != nil {
		t.Fatal(err)
	}
	shape := claimShape(claims)
	if shape != "extra:object,iat:number,list:array,name:string,none:null,sub:string,verified:bool" {
		t.Fatalf("shape = %q", shape)
	}
	for _, value := range []string{"secret-sub", "John", "1700000000"} {
		if strings.Contains(shape, value) {
			t.Fatalf("shape leaks %q", value)
		}
	}
}
