describe("engine dataset playground", () => {
  const initial = {
    mode: "synthetic",
    run_id: "run-1",
    interval_ms: 1000,
    points_per_interval: 1,
    loop: true,
    random_seed: 7,
  };

  beforeEach(() => {
    cy.intercept("GET", "/config.json", {
      body: {
        gensetIds: [], propulsionIds: [], batteryIds: [], auxloadIds: [],
        shorePower: false, shipEnabled: false,
        dataset: {
          enabled: true, collectorEnabled: true, agentNodeName: "worker-1",
          grafanaDashboardUrl: "http://localhost:3000/d/engine-dataset",
        },
      },
    });
    cy.intercept("GET", "/api/dataset/status", {
      body: { config: initial, state: "running", published: 42, last_error: null },
    }).as("status");
    cy.intercept("GET", "/api/dataset/collector/ready", {
      body: { connected: true, received: 42, dropped: 0, last_error: null },
    }).as("collector");
  });

  it("shows status without ship controls and applies replay settings after confirmation", () => {
    cy.intercept("PUT", "/api/dataset/config", (request) => {
      expect(request.body).to.deep.equal({
        mode: "real", interval_ms: 500, points_per_interval: 2, loop: true, random_seed: 7,
      });
      request.reply({ body: { ...initial, ...request.body } });
    }).as("update");
    cy.visit("/");
    cy.wait(["@status", "@collector"]);
    cy.contains("run-1").should("be.visible");
    cy.contains("worker-1").should("be.visible");
    cy.contains("Connected").should("be.visible");
    cy.contains("switchboard").should("not.exist");
    cy.contains("Open dataset telemetry dashboard").should("have.attr", "href", "http://localhost:3000/d/engine-dataset");
    cy.get("#dataset-mode").select("real");
    cy.get("#dataset-seed").should("be.disabled");
    cy.get("#dataset-interval").clear().type("500");
    cy.get("#dataset-points").clear().type("2");
    cy.on("window:confirm", (message) => {
      expect(message).to.contain("restarts it from the beginning");
      return true;
    });
    cy.contains("button", "Apply settings").click();
    cy.wait("@update");
  });

  it("reports streamer and collector errors separately", () => {
    cy.intercept("GET", "/api/dataset/status", {
      body: { config: initial, state: "error", published: 0, last_error: "PostgreSQL unavailable" },
    });
    cy.intercept("GET", "/api/dataset/collector/ready", {
      statusCode: 503,
      body: { connected: false, received: 0, dropped: 1, last_error: "Kafka unavailable" },
    });
    cy.visit("/");
    cy.contains("Stream error: PostgreSQL unavailable").should("be.visible");
    cy.contains("Collector error: Kafka unavailable").should("be.visible");
    cy.contains("Not ready").should("be.visible");
  });

  it("does not submit invalid input or a cancelled change", () => {
    cy.intercept("PUT", "/api/dataset/config", { statusCode: 200 }).as("update");
    cy.visit("/");
    cy.wait("@status");
    cy.get("#dataset-interval").clear().type("9");
    cy.contains("button", "Apply settings").click();
    cy.get("@update.all").should("have.length", 0);
    cy.get("#dataset-interval").clear().type("1000");
    cy.on("window:confirm", () => false);
    cy.contains("button", "Apply settings").click();
    cy.get("@update.all").should("have.length", 0);
  });

  it("reports failed writes and stream outages", () => {
    cy.intercept("PUT", "/api/dataset/config", { statusCode: 401, body: "Unauthorized" }).as("update");
    cy.visit("/");
    cy.wait("@status");
    cy.on("window:confirm", () => true);
    cy.contains("button", "Apply settings").click();
    cy.wait("@update");
    cy.contains("Could not apply settings").should("contain", "401");
    cy.intercept("GET", "/api/dataset/status", { statusCode: 503, body: "Unavailable" });
    cy.contains("Stream status:", { timeout: 5000 }).should("be.visible");
    cy.contains("button", "Apply settings").should("be.disabled");
  });
});
