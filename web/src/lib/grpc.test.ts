import { describe, it, expect } from "vitest";
import { grpcStatusName, grpcStatusText, grpcAddress } from "./grpc";

describe("grpc helpers", () => {
  it("names the 17 canonical status codes", () => {
    expect(grpcStatusName(0)).toBe("OK");
    expect(grpcStatusName(14)).toBe("UNAVAILABLE");
    expect(grpcStatusName(16)).toBe("UNAUTHENTICATED");
    expect(grpcStatusName(99)).toBe("");
  });
  it("shows the code with its name, and an unknown code as it is", () => {
    expect(grpcStatusText(4)).toBe("4 DEADLINE_EXCEEDED");
    expect(grpcStatusText(99)).toBe("99");
    expect(grpcStatusText(null)).toBe("");
    expect(grpcStatusText(undefined)).toBe("");
  });
  it("the address a gRPC client dials is host:443", () => {
    expect(grpcAddress("grpc.example.com")).toBe("grpc.example.com:443");
    expect(grpcAddress("*.example.com")).toBe("");
  });
});
