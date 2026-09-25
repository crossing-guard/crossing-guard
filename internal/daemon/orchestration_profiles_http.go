package daemon

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"crossing-guard/internal/orchestration/profilefs"
)

const orchestrationProfileRequestLimit = 360 * 1024

func registerOrchestrationProfileRoutes(mux *http.ServeMux, owner *profilefs.Owner) {
	mux.HandleFunc("GET /api/orchestration/profiles", func(w http.ResponseWriter, _ *http.Request) {
		profiles, err := owner.List()
		if err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		writeJSON(w, profiles)
	})
	mux.HandleFunc("GET /api/orchestration/profiles/{id}", func(w http.ResponseWriter, r *http.Request) {
		profile, err := owner.Get(r.PathValue("id"))
		if err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		writeJSON(w, profile)
	})
	mux.HandleFunc("POST /api/orchestration/profiles/preview", func(w http.ResponseWriter, r *http.Request) {
		request, err := decodeProfileSourceRequest(w, r)
		if err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		preview, err := owner.Preview(request.SourceName, request.Source)
		if err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		writeJSON(w, map[string]any{"preview": preview,
			"note": "Preview is read-only. No profile was selected or run."})
	})
	mux.HandleFunc("POST /api/orchestration/profiles/select", func(w http.ResponseWriter, r *http.Request) {
		request, err := decodeProfileSelectRequest(w, r)
		if err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		result, err := owner.Select(profilefs.SelectCommand{SourceName: request.SourceName, Source: request.Source,
			ExpectedSourceDigest: request.SourceDigest, ExpectedBundleDigest: request.BundleDigest,
			ExpectedStateToken: request.StateToken})
		if err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		writeJSON(w, result)
	})
}

type decodedProfileSource struct {
	SourceName string
	Source     []byte
}

type decodedProfileSelect struct {
	decodedProfileSource
	SourceDigest string
	BundleDigest string
	StateToken   string
}

func decodeProfileSourceRequest(w http.ResponseWriter, r *http.Request) (decodedProfileSource, error) {
	var request struct {
		SourceName   string `json:"source_name"`
		SourceBase64 string `json:"source_base64"`
	}
	if err := decodeBoundedProfileJSON(w, r, &request); err != nil {
		return decodedProfileSource{}, err
	}
	source, err := base64.StdEncoding.Strict().DecodeString(request.SourceBase64)
	if err != nil {
		return decodedProfileSource{}, profileTransportProblem("source_base64",
			"The encoded profile source is invalid.", "Choose PROFILE.md again and retry.")
	}
	if len(source) > profilefs.MaxSourceBytes {
		return decodedProfileSource{}, &profilefs.Problem{Code: "source_too_large", Field: "source",
			Message: "The profile source exceeds 262144 bytes.", Recovery: "Reduce PROFILE.md and preview it again."}
	}
	return decodedProfileSource{SourceName: request.SourceName, Source: source}, nil
}

func decodeProfileSelectRequest(w http.ResponseWriter, r *http.Request) (decodedProfileSelect, error) {
	var request struct {
		SourceName   string `json:"source_name"`
		SourceBase64 string `json:"source_base64"`
		SourceDigest string `json:"source_digest"`
		BundleDigest string `json:"bundle_digest"`
		StateToken   string `json:"state_token"`
		Confirmed    bool   `json:"confirmed"`
	}
	if err := decodeBoundedProfileJSON(w, r, &request); err != nil {
		return decodedProfileSelect{}, err
	}
	if !request.Confirmed {
		return decodedProfileSelect{}, profileTransportProblem("confirmed", "Explicit selection confirmation is required.",
			"Review the preview and select the exact revision explicitly.")
	}
	source, err := base64.StdEncoding.Strict().DecodeString(request.SourceBase64)
	if err != nil {
		return decodedProfileSelect{}, profileTransportProblem("source_base64",
			"The encoded profile source is invalid.", "Choose PROFILE.md again and retry.")
	}
	if len(source) > profilefs.MaxSourceBytes {
		return decodedProfileSelect{}, &profilefs.Problem{Code: "source_too_large", Field: "source",
			Message: "The profile source exceeds 262144 bytes.", Recovery: "Reduce PROFILE.md and preview it again."}
	}
	return decodedProfileSelect{decodedProfileSource: decodedProfileSource{SourceName: request.SourceName, Source: source},
		SourceDigest: request.SourceDigest, BundleDigest: request.BundleDigest, StateToken: request.StateToken}, nil
}

func decodeBoundedProfileJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	r.Body = http.MaxBytesReader(w, r.Body, orchestrationProfileRequestLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return &profilefs.Problem{Code: "request_too_large", Message: "The profile request is too large.",
				Recovery: "Reduce PROFILE.md and retry."}
		}
		return profileTransportProblem("request", "The profile request JSON is invalid.",
			"Send exactly the documented profile request fields.")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return profileTransportProblem("request", "The profile request contains trailing JSON.",
			"Send exactly one JSON request value.")
	}
	return nil
}

func profileTransportProblem(field, message, recovery string) error {
	return &profilefs.Problem{Code: "invalid_request", Field: field, Message: message, Recovery: recovery}
}

func writeOrchestrationProfileError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var problem *profilefs.Problem
	if !errors.As(err, &problem) {
		problem = &profilefs.Problem{Code: "storage_error", Message: "Profile storage is unavailable.",
			Recovery: "Review local profile storage diagnostics and retry."}
	}
	switch problem.Code {
	case "invalid_request", "invalid_source_name":
		status = http.StatusBadRequest
	case "request_too_large", "source_too_large", "frontmatter_too_large", "instructions_too_large":
		status = http.StatusRequestEntityTooLarge
	case "invalid_profile", "incompatible_profile":
		status = http.StatusUnprocessableEntity
	case "not_found":
		status = http.StatusNotFound
	case "state_conflict", "integrity_conflict":
		status = http.StatusConflict
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"error": problem})
}
