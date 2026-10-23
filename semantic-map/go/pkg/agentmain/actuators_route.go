package agentmain

import (
	"net/http"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/runtime"
)

// actuatorView is one action of GET /actuators: what it does, and the changes the
// agent holds through it. Can is present only for ?subject=: why the action
// cannot apply to that workload, "" when it can.
type actuatorView struct {
	Descriptor actuators.Descriptor `json:"descriptor"`
	Held       []actuators.Entry    `json:"held"`
	Can        *string              `json:"can,omitempty"`
}

// registerActuatorsRoute serves the actions the agent can take, each with the
// ledger's entries for it. ?subject= narrows the entries to one workload and
// adds whether each action can apply to it, which the runtime must list.
func registerActuatorsRoute(mux *http.ServeMux, rt runtime.Runtime, acting *actuators.Acting) {
	mux.HandleFunc("GET /actuators", func(w http.ResponseWriter, r *http.Request) {
		subject := r.URL.Query().Get("subject")
		var wl *runtime.Workload
		if subject != "" {
			ws, err := rt.Workloads(r.Context())
			if err != nil {
				writeError(w, http.StatusBadGateway, "listing workloads: "+err.Error())
				return
			}
			for i := range ws {
				if ws[i].Subject == subject {
					wl = &ws[i]
				}
			}
			if wl == nil {
				writeError(w, http.StatusNotFound, "the runtime has no workload "+subject)
				return
			}
		}
		out := []actuatorView{}
		for _, d := range acting.Descriptors() {
			v := actuatorView{Descriptor: d, Held: []actuators.Entry{}}
			for _, e := range acting.Entries(d.Action) {
				if subject == "" || e.Subject == subject {
					v.Held = append(v.Held, e)
				}
			}
			if wl != nil {
				a, _ := acting.Actuator(d.Action)
				why := a.Check(*wl)
				v.Can = &why
			}
			out = append(out, v)
		}
		writeJSON(w, out)
	})
}
