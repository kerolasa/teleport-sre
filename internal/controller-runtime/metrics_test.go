package controllerruntime

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestHeartbeat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// broken is the reason the api server does not answer, and an empty
		// one means the cluster answers as it should.
		broken     error
		wantStatus int
		wantBody   string
	}{
		{
			name:       "the api server answers",
			wantStatus: http.StatusOK,
			wantBody:   aliveBody,
		},
		{
			name:       "the api server does not answer",
			broken:     errors.New("the api server is not there"),
			wantStatus: http.StatusInternalServerError,
			wantBody:   notAliveBody,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// The heartbeat asks for a namespace, so the fake is broken the
			// way a cluster that cannot be reached breaks that listing.
			client := fake.NewClientset()
			if test.broken != nil {
				client.PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, test.broken
				})
			}

			handler := ListenMetrics(Conf{}, KubeConn{k8s: client}).Handler
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, heartbeatPath, nil))

			if recorder.Code != test.wantStatus {
				t.Errorf("GET %s status = %d, want %d", heartbeatPath, recorder.Code, test.wantStatus)
			}
			if body := recorder.Body.String(); body != test.wantBody {
				t.Errorf("GET %s body = %q, want %q", heartbeatPath, body, test.wantBody)
			}
		})
	}
}
