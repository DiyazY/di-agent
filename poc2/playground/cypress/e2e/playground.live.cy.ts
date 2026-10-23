// Exercises the playground against a real deployment: the Kubernetes
// cluster must be up with genset, battery, propulsion, auxload, switchboard
// (and optionally shore-power) services running and reachable at the
// configured baseUrl. Unlike playground.cy.ts, nothing here is stubbed with
// cy.intercept - every request hits the live backend, so tests read the
// actual /config.json instead of assuming fixed instance ids/counts.
describe("di-agent playground (live cluster)", () => {
  const SYSTEMS: Array<{ system: string; configKey: string }> = [
    { system: "genset", configKey: "gensetIds" },
    { system: "battery", configKey: "batteryIds" },
    { system: "propulsion", configKey: "propulsionIds" },
    { system: "auxload", configKey: "auxloadIds" },
  ];

  it("loads the real deployment config and renders one panel per instance", () => {
    cy.request("/config.json").then(({ body: config }) => {
      cy.visit("/");
      cy.contains("h1", "di-agent Playground").should("be.visible");
      cy.contains("switchboard").should("be.visible");
      SYSTEMS.forEach(({ configKey }) => {
        (config[configKey] as string[]).forEach((id) => {
          cy.contains(id).should("be.visible");
        });
      });
    });
  });

  SYSTEMS.forEach(({ system, configKey }) => {
    it(`${system} instances report healthy status and valid load ratios from the live API`, () => {
      cy.request("/config.json").then(({ body: config }) => {
        const ids = config[configKey] as string[];
        expect(ids, `${configKey} in /config.json`).to.be.an("array");
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

  it("toggling anomaly mode on a live genset instance flips anomaly_enabled and can be reverted", () => {
    cy.request("/config.json").then(({ body: config }) => {
      const id = (config.gensetIds as string[])[0];
      if (!id) {
        cy.log("No genset instances deployed, skipping.");
        return;
      }

      cy.request(`/api/genset/${id}/status`).its("body.anomaly_enabled").then((original) => {
        cy.visit("/");
        cy.contains(id)
          .parents(".panel")
          .within(() => {
            cy.contains("button", original ? "Disable" : "Enable").click();
          });

        cy.request(`/api/genset/${id}/status`)
          .its("body.anomaly_enabled")
          .should("eq", !original)
          .then(() => {
            // Revert so the live instance is left in its original state.
            cy.contains(id)
              .parents(".panel")
              .within(() => {
                cy.contains("button", original ? "Enable" : "Disable").click();
              });
            cy.request(`/api/genset/${id}/status`)
              .its("body.anomaly_enabled")
              .should("eq", original);
          });
      });
    });
  });

  it("setting a new load ratio on a live propulsion instance updates its target and the switchboard demand", () => {
    cy.request("/config.json").then(({ body: config }) => {
      const id = (config.propulsionIds as string[])[0];
      if (!id) {
        cy.log("No propulsion instances deployed, skipping.");
        return;
      }

      cy.request(`/api/propulsion/${id}/status`).its("body.target_load_ratio").then((original) => {
        cy.visit("/");
        cy.contains(id)
          .parents(".panel")
          .within(() => {
            cy.get("input[type=range]")
              .invoke("val", 30)
              .trigger("input")
              .trigger("change");
            cy.contains("button", "Apply").click();
          });

        cy.contains(id)
          .parents(".panel")
          .within(() => {
            cy.contains("Target load").parent().should("contain", "30.0%");
          });
        cy.request(`/api/propulsion/${id}/status`)
          .its("body.target_load_ratio")
          .should("be.closeTo", 0.3, 0.01);

        // Switchboard polls independently, so give it a cycle to pick up the change.
        cy.request("/api/switchboard/status")
          .its("body.consumers")
          .its(id)
          .its("requested_power_kw")
          .should("be.a", "number");

        // Restore the original target so the live instance isn't left altered.
        cy.contains(id)
          .parents(".panel")
          .within(() => {
            cy.get("input[type=range]")
              .invoke("val", Math.round(original * 100))
              .trigger("input")
              .trigger("change");
            cy.contains("button", "Apply").click();
          });
        cy.request(`/api/propulsion/${id}/status`)
          .its("body.target_load_ratio")
          .should("be.closeTo", original, 0.01);
      });
    });
  });
});
