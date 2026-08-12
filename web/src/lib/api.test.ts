// @vitest-environment jsdom

import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./api";

describe("api.importCharacterFile", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("does not force json content-type for FormData uploads", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify({ id: "c1", name: "测试角色", world_entries: 0 }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );

    await api.importCharacterFile(
      new File(['{"spec":"chara_card_v2"}'], "card.json", { type: "application/json" }),
    );

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [, options] = fetchMock.mock.calls[0];
    expect(options?.body).toBeInstanceOf(FormData);
    expect(options?.headers).not.toMatchObject({ "Content-Type": "application/json" });
  });
});
