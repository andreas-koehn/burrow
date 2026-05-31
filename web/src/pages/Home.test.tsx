import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";
import { renderApp } from "@/mocks/test-utils";
import Home from "./Home";

describe("Home (Overview)", () => {
  it("renders the Overview heading", async () => {
    renderApp(<Home />);
    expect(await screen.findByRole("heading", { name: "Overview" })).toBeInTheDocument();
  });
});
