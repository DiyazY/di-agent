// Exercises the playground against a real deployment: the Kubernetes
// cluster must be up with genset, battery, propulsion, auxload, switchboard
// (and optionally shore-power) services running and reachable at the
// configured baseUrl. Intercepts observe requests without stubbing responses,
// and tests read the actual /config.json instead of assuming instance IDs.
describe("di-agent playground (live cluster)", () => {
  let restore: { url: string; body: object } | undefined;
  const SYSTEMS: Array<{ system: string; configKey: string }> = [
    { system: "genset", configKey: "gensetIds" },
    { system: "battery", configKey: "batteryIds" },
    { system: "propulsion", configKey: "propulsionIds" },
    { system: "auxload", configKey: "auxloadIds" },
  ];

  function waitForDemand(id: string, predicate: (value: number) => boolean, deadline: number): Cypress.Chainable<null> {
    return cy.request("/api/switchboard/status").then<null>(({ body }) => {
      const demand = body.consumers?.[id]?.requested_power_kw;
      if (typeof demand === "number" && predicate(demand)) return cy.wrap(null, { log: false });
      if (Date.now() >= deadline) {
        throw new Error(`Switchboard demand for ${id} did not reach the expected value: ${demand}`);
      }
      return cy.wait(500).then(() => waitForDemand(id, predicate, deadline));
    });
  }

  afterEach(() => {
    if (restore) {
      cy.request("POST", restore.url, restore.body);
      restore = undefined;
    }
  });

  it("loads the real deployment config and renders one panel per instance", () => {
    cy.request("/config.json").then(({ body: config }) => {
      cy.visit("/");
      cy.contains("h1", "di-agent Playground").should("be.visible");
      cy.contains("switchboard").should("be.visible");
      SYSTEMS.forEach(({ configKey }) => {
        expect(config[configKey], configKey).to.be.an("array").and.not.be.empty;
        (config[configKey] as string[]).forEach((id) => {
          cy.contains("h2", id).should("be.visible");
        });
      });
    });
  });

  SYSTEMS.forEach(({ system, configKey }) => {
    it(`${system} instances report healthy status and valid load ratios from the live API`, () => {
      cy.request("/config.json").then(({ body: config }) => {
        const ids = config[configKey] as string[];
        expect(ids, `${configKey} in /config.json`).to.be.an("array").and.not.be.empty;
        ids.forEach((id) => {
          cy.request(`/api/${system}/${id}/health`)
            .its("body.status")
            .should("eq", "ok");
          cy.request(`/api/${system}/${id}/status`).its("body").then((status) => {
            expect(status.target_load_ratio).to.be.within(0, 1);
            expect(status.current_load_ratio).to.be.within(0, 1);
          });
        });
      });
    });
  });

  it("battery instances additionally report a valid state of charge", () => {
    cy.request("/config.json").then(({ body: config }) => {
      expect(config.batteryIds, "deployed batteries").to.be.an("array").and.not.be.empty;
      (config.batteryIds as string[]).forEach((id) => {
        cy.request(`/api/battery/${id}/status`).its("body.soc").should("be.within", 0, 1);
      });
    });
  });

  it("switchboard reports healthy status and non-negative power/emissions figures", () => {
    cy.request("/api/switchboard/health").its("body.status").should("eq", "ok");
    cy.request("/api/switchboard/status").its("body").then((status) => {
      expect(status.available_supply_kw).to.be.at.least(0);
      expect(status.total_demand_kw).to.be.at.least(0);
      expect(status.total_co2_kg_per_s).to.be.at.least(0);
      expect(status.total_nox_kg_per_s).to.be.at.least(0);
    });
  });

  it("toggling anomaly mode on a live genset instance flips anomaly_enabled", () => {
    cy.request("/config.json").then(({ body: config }) => {
      const id = (config.gensetIds as string[])[0];
      expect(id, "a deployed genset").to.be.a("string").and.not.be.empty;

      cy.request(`/api/genset/${id}/status`).its("body.anomaly_enabled").then((original) => {
        restore = { url: `/api/genset/${id}/anomaly`, body: { enabled: original } };
        cy.intercept("POST", `/api/genset/${id}/anomaly`).as("anomaly");
        cy.visit("/");
        cy.contains("h2", id)
          .closest(".panel")
          .within(() => {
            cy.contains("button", original ? "Disable" : "Enable").click();
          });

        cy.wait("@anomaly").its("response.statusCode").should("eq", 200);
        cy.request(`/api/genset/${id}/status`)
          .its("body.anomaly_enabled")
          .should("eq", !original);
      });
    });
  });

  it("setting a new propulsion load updates its target and switchboard demand", () => {
    cy.request("/config.json").then(({ body: config }) => {
      const id = (config.propulsionIds as string[])[0];
      expect(id, "a deployed propulsion instance").to.be.a("string").and.not.be.empty;

      cy.request(`/api/propulsion/${id}/status`).its("body.target_load_ratio").then((original) => {
        restore = { url: `/api/propulsion/${id}/load`, body: { load_ratio: original } };
        const target = 0.3;
        cy.request("POST", `/api/propulsion/${id}/load`, { load_ratio: 0 });
        cy.then(() => waitForDemand(id, (value) => value < 1, Date.now() + 45000));
        cy.intercept("POST", `/api/propulsion/${id}/load`).as("load");
        cy.visit("/");
        cy.contains("h2", id)
          .closest(".panel")
          .within(() => {
            cy.get("input[type=range]").should("not.be.disabled").then(($input) => {
              const input = $input[0] as HTMLInputElement;
              const win = input.ownerDocument.defaultView!;
              const setValue = Object.getOwnPropertyDescriptor(win.HTMLInputElement.prototype, "value")?.set;
              if (!setValue) throw new Error("Cannot set the range input value");
              setValue.call(input, String(target * 100));
              input.dispatchEvent(new win.Event("input", { bubbles: true }));
            });
            cy.contains("button", "Apply").click();
          });

        cy.contains("h2", id)
          .closest(".panel")
          .within(() => {
            cy.contains("Target load").parent().should("contain", `${(target * 100).toFixed(1)}%`);
          });
        cy.wait("@load").its("response.statusCode").should("eq", 200);
        cy.request(`/api/propulsion/${id}/status`)
          .its("body.target_load_ratio")
          .should("be.closeTo", target, 0.01);
        cy.then(() => waitForDemand(id, (value) => value > 1, Date.now() + 45000));
      });
    });
  });
});
