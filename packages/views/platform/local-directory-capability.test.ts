// @vitest-environment node

import { describe, expect, it } from "vitest";
import { resolveLocalDirectoryCapability } from "./local-directory";

// The three-tier gate behind every local-directory add flow, asserted as a
// pure matrix so component tests don't each re-probe the same order. The
// tiers answer "what can complete this action here", never "which platform is
// this": a desktop shell keeps the native picker even when the server gains
// the browse endpoint, and web gains server browsing exactly when the config
// boolean flips true.
describe("resolveLocalDirectoryCapability", () => {
  it("keeps the native desktop picker on top, whatever else is available", () => {
    expect(
      resolveLocalDirectoryCapability({
        desktopPickerAvailable: true,
        serverBrowserSupported: true,
        localDaemonAvailable: true,
      }),
    ).toBe("desktop_picker");
  });

  it("falls to the server browser when only that capability exists (web, new server)", () => {
    expect(
      resolveLocalDirectoryCapability({
        desktopPickerAvailable: false,
        serverBrowserSupported: true,
        localDaemonAvailable: false,
      }),
    ).toBe("server_browser");
  });

  // Manual entry is only an escape hatch where a locally registered daemon
  // exists to bind the resource to; without one, submitting could never
  // produce a valid daemon_id.
  it("falls to manual path entry when a local daemon exists but nothing else", () => {
    expect(
      resolveLocalDirectoryCapability({
        desktopPickerAvailable: false,
        serverBrowserSupported: false,
        localDaemonAvailable: true,
      }),
    ).toBe("manual_path");
  });

  it("is read-only when no tier can complete the binding (web, old server)", () => {
    expect(
      resolveLocalDirectoryCapability({
        desktopPickerAvailable: false,
        serverBrowserSupported: false,
        localDaemonAvailable: false,
      }),
    ).toBe("read_only");
  });
});
