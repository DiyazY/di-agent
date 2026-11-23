describe("engine dataset playground (live cluster)", () => {
  it("applies replay pacing through the deployed UI and backend", () => {
    cy.intercept("PUT", "/api/dataset/config").as("update");
    cy.visit("/");
    cy.contains("h2", "Engine dataset").should("be.visible");
    cy.contains("ci-replay").should("be.visible");
    cy.contains(".metric-row", "Collector").should("contain", "Connected");
    cy.get("#dataset-interval").clear().type("750");
    cy.on("window:confirm", () => true);
    cy.contains("button", "Apply settings").click();
    cy.wait("@update").its("response.statusCode").should("eq", 200);
    cy.request("/api/dataset/status").its("body.config.interval_ms").should("eq", 750);
    cy.contains("Active pacing").parent().should("contain", "750 ms");
  });
});
