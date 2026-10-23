describe("di-agent playground", () => {
  let shorePowerConnected = false;

  beforeEach(() => {
    shorePowerConnected = false;

    cy.intercept("GET", "/config.json", {
      body: {
        gensetIds: ["genset-1"],
        propulsionIds: ["propulsion-1"],
        batteryIds: ["battery-1"],
        auxloadIds: ["auxload-1"],
        shorePower: true,
      },
    });

    cy.intercept("GET", "/api/**/health", { body: { status: "ok" } });
    cy.intercept("GET", "/api/switchboard/status", {
      body: {
        switchboard_id: "switchboard-1",
        available_supply_kw: 500,
        total_demand_kw: 120,
        total_co2_kg_per_s: 0.01,
        total_nox_kg_per_s: 0.001,
        gensets: {},
        batteries: {},
        consumers: {},
        data_quality: {
          total_sources: 0,
          stale_sources: 0,
          total_consumers: 0,
          stale_consumers: 0,
        },
      },
    });
    cy.intercept("GET", "/api/shore-power/status", () => ({
      body: {
        shore_power_id: "shore-power-1",
        connected: shorePowerConnected,
        target_power_ratio: 0.5,
        current_power_ratio: 0.5,
        last_message: { power_kw: 50, losses_kw: 1 },
      },
    }));
    cy.intercept("GET", "/api/*/*/status", (request) => {
      const pathParts = request.url.split("/");
      const system = pathParts[pathParts.length - 3];
      const status = {
        target_load_ratio: 0.2,
        current_load_ratio: 0.2,
        anomaly_enabled: false,
        ...(system === "battery" ? { soc: 0.8, time_to_full_hr: 2 } : {}),
        ...(system === "propulsion" || system === "auxload"
          ? { allocated_power_kw: 40 }
          : {}),
      };
      request.reply({ body: status });
    });
    cy.intercept("POST", "/api/**", (request) => {
      if (request.url.endsWith("/shore-power/connect")) {
        shorePowerConnected = request.body.connected;
        request.reply({ body: { connected: shorePowerConnected } });
        return;
      }
      if (request.url.includes("/propulsion/") && request.url.endsWith("/load")) {
        request.alias = "propulsionLoad";
      }
      if (request.url.includes("/auxload/") && request.url.endsWith("/load")) {
        request.alias = "auxloadLoad";
      }
      request.reply({ body: {} });
    }).as("apiCommand");
  });

  it("renders the deployed panels from config", () => {
    cy.visit("/");

    cy.contains("h1", "di-agent Playground").should("be.visible");
    cy.contains("genset-1").should("be.visible");
    cy.contains("propulsion-1").should("be.visible");
    cy.contains("battery-1").should("be.visible");
    cy.contains("auxload-1").should("be.visible");
    cy.contains("shore-power").should("be.visible");
    cy.contains("switchboard").should("be.visible");
    cy.contains("voyage conditions").should("be.visible");
  });

  it("calculates and applies voyage recommendations", () => {
    cy.visit("/");

    cy.get("#ocean-state-select").select("storm");
    cy.contains("Recommended propulsion load").parent().should("contain", "90%");
    cy.contains("Recommended auxiliary load").parent().should("contain", "20%");

    cy.contains("button", "Apply recommendations").click();
    cy.wait(["@propulsionLoad", "@auxloadLoad"]).then((requests) => {
      expect(requests.map(({ request }) => request.body)).to.deep.include.members([
        { load_ratio: 0.9 },
        { load_ratio: 0.2 },
      ]);
    });
    cy.contains("Recommendations applied.").should("be.visible");
  });
});