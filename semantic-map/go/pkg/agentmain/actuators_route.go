package agentmain

import (
	"net/http"

	"github.com/DiyazY/di-agent/pkg/actuators"
	"github.com/DiyazY/di-agent/pkg/runtime"
)

// actuatorView is one action of GET /actuators: what it does, the changes the
// agent holds through it and gives back (Held: this node's, of a built action),
// and the changes the ledger holds of it that nothing in this agent gives back,
// each with why (Unmanaged: another node's, an action no lever runs, or a real
// run's while this one is dry). Built is false for an action the ledger holds
// changes of and no lever runs (relief off, or the lever removed): it has no
// descriptor and no can. Can is present only for ?subject= on a built action:
// why the action cannot apply to that workload, "" when it can.
type actuatorView struct {
	Action     string                `json:"action"`
	Built      bool                  `json:"built"`
	Descriptor *actuators.Descriptor `json:"descriptor,omitempty"`
	Held       []actuators.Entry     `json:"held"`
	Unmanaged  []actuators.Unmanaged `json:"unmanaged"`
	Can        *string               `json:"can,omitempty"`
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
		row := map[string]int{}
		for _, d := range acting.Descriptors() {
			v := actuatorView{Action: d.Action, Built: true, Descriptor: &d, Held: []actuators.Entry{}, Unmanaged: []actuators.Unmanaged{}}
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
			row[d.Action] = len(out)
			out = append(out, v)
		}
		// Changes nothing here gives back: under their built action, or one row
		// per action no lever runs, after the built ones.
		for _, u := range acting.Unmanaged() {
			if subject != "" && u.Subject != subject {
				continue
			}
			i, ok := row[u.Action]
			if !ok {
				i = len(out)
				row[u.Action] = i
				out = append(out, actuatorView{Action: u.Action, Held: []actuators.Entry{}, Unmanaged: []actuators.Unmanaged{}})
			}
			out[i].Unmanaged = append(out[i].Unmanaged, u)
		}
		writeJSON(w, out)
	})
}
