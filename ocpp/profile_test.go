package ocpp

import (
	"reflect"
	"testing"
)

type fakeFeature struct {
	name string
}

func (f *fakeFeature) GetFeatureName() string {
	return f.name
}

func (f *fakeFeature) GetRequestType() reflect.Type {
	return reflect.TypeOf(fakeRequest{})
}

func (f *fakeFeature) GetResponseType() reflect.Type {
	return reflect.TypeOf(fakeResponse{})
}

type fakeRequest struct {
	name string
}

func (r fakeRequest) GetFeatureName() string {
	return r.name
}

type fakeResponse struct {
	name string
}

func (r fakeResponse) GetFeatureName() string {
	return r.name
}

func TestNewProfile(t *testing.T) {
	t.Run("registers all supplied features", func(t *testing.T) {
		features := []*fakeFeature{
			{name: "FeatureA"},
			{name: "FeatureB"},
			{name: "FeatureC"},
		}

		profile := NewProfile("test-profile", features[0], features[1], features[2])

		if profile.Name != "test-profile" {
			t.Fatalf("NewProfile() name = %q, want %q", profile.Name, "test-profile")
		}

		for _, feature := range features {
			if !profile.SupportsFeature(feature.name) {
				t.Errorf("SupportsFeature(%q) = false, want true", feature.name)
			}
			if got := profile.GetFeature(feature.name); got != feature {
				t.Errorf("GetFeature(%q) = %v, want %v", feature.name, got, feature)
			}
		}
	})

	t.Run("creates empty feature registry with no features", func(t *testing.T) {
		profile := NewProfile("empty-profile")

		if profile.Name != "empty-profile" {
			t.Fatalf("NewProfile() name = %q, want %q", profile.Name, "empty-profile")
		}
		if len(profile.Features) != 0 {
			t.Fatalf("len(Features) = %d, want 0", len(profile.Features))
		}
		if profile.SupportsFeature("missing") {
			t.Error("SupportsFeature(\"missing\") = true, want false")
		}
		if got := profile.GetFeature("missing"); got != nil {
			t.Errorf("GetFeature(\"missing\") = %v, want nil", got)
		}
	})
}

func TestProfileAddFeature(t *testing.T) {
	t.Run("registers under feature name", func(t *testing.T) {
		profile := NewProfile("test-profile")
		feature := &fakeFeature{name: "FeatureA"}

		profile.AddFeature(feature)

		if !profile.SupportsFeature("FeatureA") {
			t.Fatal("SupportsFeature(\"FeatureA\") = false, want true")
		}
		if got := profile.GetFeature("FeatureA"); got != feature {
			t.Errorf("GetFeature(\"FeatureA\") = %v, want %v", got, feature)
		}
	})

	t.Run("overwrites existing feature with same name", func(t *testing.T) {
		profile := NewProfile("test-profile")
		first := &fakeFeature{name: "FeatureA"}
		second := &fakeFeature{name: "FeatureA"}

		profile.AddFeature(first)
		profile.AddFeature(second)

		if got := profile.GetFeature("FeatureA"); got != second {
			t.Errorf("GetFeature(\"FeatureA\") = %v, want %v", got, second)
		}
	})
}

func TestProfileSupportsFeature(t *testing.T) {
	profile := NewProfile("test-profile", &fakeFeature{name: "FeatureA"})

	tests := []struct {
		name string
		want bool
	}{
		{name: "FeatureA", want: true},
		{name: "UnknownFeature", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := profile.SupportsFeature(tt.name); got != tt.want {
				t.Errorf("SupportsFeature(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestProfileGetFeature(t *testing.T) {
	feature := &fakeFeature{name: "FeatureA"}
	profile := NewProfile("test-profile", feature)

	tests := []struct {
		name string
		want Feature
	}{
		{name: "FeatureA", want: feature},
		{name: "UnknownFeature", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := profile.GetFeature(tt.name); got != tt.want {
				t.Errorf("GetFeature(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestProfileParseRequest(t *testing.T) {
	feature := &fakeFeature{name: "FeatureA"}
	profile := NewProfile("test-profile", feature)
	rawRequest := struct{ payload string }{payload: "request"}
	wantRequest := fakeRequest{name: "FeatureA"}

	t.Run("supported feature invokes parser with request type", func(t *testing.T) {
		var called bool
		var gotRaw interface{}
		var gotType reflect.Type

		gotRequest, err := profile.ParseRequest("FeatureA", rawRequest, func(raw interface{}, requestType reflect.Type) (Request, error) {
			called = true
			gotRaw = raw
			gotType = requestType
			return wantRequest, nil
		})

		if err != nil {
			t.Fatalf("ParseRequest() error = %v, want nil", err)
		}
		if !called {
			t.Fatal("request parser was not called")
		}
		if gotRaw != rawRequest {
			t.Errorf("request parser raw = %v, want %v", gotRaw, rawRequest)
		}
		if gotType != feature.GetRequestType() {
			t.Errorf("request parser type = %v, want %v", gotType, feature.GetRequestType())
		}
		if gotRequest != wantRequest {
			t.Errorf("ParseRequest() request = %v, want %v", gotRequest, wantRequest)
		}
	})

	t.Run("unsupported feature returns error without invoking parser", func(t *testing.T) {
		var called bool

		gotRequest, err := profile.ParseRequest("UnknownFeature", rawRequest, func(raw interface{}, requestType reflect.Type) (Request, error) {
			called = true
			return fakeRequest{}, nil
		})

		if err == nil {
			t.Fatal("ParseRequest() error = nil, want non-nil")
		}
		if gotRequest != nil {
			t.Errorf("ParseRequest() request = %v, want nil", gotRequest)
		}
		if called {
			t.Error("request parser was called for unsupported feature")
		}
	})
}

func TestProfileParseResponse(t *testing.T) {
	feature := &fakeFeature{name: "FeatureA"}
	profile := NewProfile("test-profile", feature)
	rawResponse := struct{ payload string }{payload: "response"}
	wantResponse := fakeResponse{name: "FeatureA"}

	t.Run("supported feature invokes parser with response type", func(t *testing.T) {
		var called bool
		var gotRaw interface{}
		var gotType reflect.Type

		gotResponse, err := profile.ParseResponse("FeatureA", rawResponse, func(raw interface{}, responseType reflect.Type) (Response, error) {
			called = true
			gotRaw = raw
			gotType = responseType
			return wantResponse, nil
		})

		if err != nil {
			t.Fatalf("ParseResponse() error = %v, want nil", err)
		}
		if !called {
			t.Fatal("response parser was not called")
		}
		if gotRaw != rawResponse {
			t.Errorf("response parser raw = %v, want %v", gotRaw, rawResponse)
		}
		if gotType != feature.GetResponseType() {
			t.Errorf("response parser type = %v, want %v", gotType, feature.GetResponseType())
		}
		if gotResponse != wantResponse {
			t.Errorf("ParseResponse() response = %v, want %v", gotResponse, wantResponse)
		}
	})

	t.Run("unsupported feature returns error without invoking parser", func(t *testing.T) {
		var called bool

		gotResponse, err := profile.ParseResponse("UnknownFeature", rawResponse, func(raw interface{}, responseType reflect.Type) (Response, error) {
			called = true
			return fakeResponse{}, nil
		})

		if err == nil {
			t.Fatal("ParseResponse() error = nil, want non-nil")
		}
		if gotResponse != nil {
			t.Errorf("ParseResponse() response = %v, want nil", gotResponse)
		}
		if called {
			t.Error("response parser was called for unsupported feature")
		}
	})
}
