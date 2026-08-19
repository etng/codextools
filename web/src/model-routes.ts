export type RelayModelRoute = {
  model: string;
  targetRelayId: string;
  targetModel: string;
};

export type RelayModelRouteProfile = {
  id: string;
  name: string;
  baseUrl: string;
  apiKey: string;
  protocol: "responses" | "chatCompletions";
  relayMode: "official" | "mixedApi" | "pureApi" | "aggregate";
  officialMixApiKey: boolean;
  modelRoutes?: RelayModelRoute[];
};

export function normalizeRelayModelRoutes(routes: RelayModelRoute[] | undefined): RelayModelRoute[] {
  if (!Array.isArray(routes)) return [];
  return routes.map((route) => ({
    model: typeof route?.model === "string" ? route.model : "",
    targetRelayId: typeof route?.targetRelayId === "string" ? route.targetRelayId : "",
    targetModel: typeof route?.targetModel === "string" ? route.targetModel : "",
  }));
}

export function relayModelRouteValidationMessage(
  source: RelayModelRouteProfile,
  profiles: RelayModelRouteProfile[],
): string {
  const sources = [source, ...profiles.filter((candidate) => candidate.id !== source.id)];
  for (const routeSource of sources) {
    if (routeSource.relayMode === "aggregate") continue;
    const seen = new Set<string>();
    for (const route of normalizeRelayModelRoutes(routeSource.modelRoutes)) {
      const model = route.model.trim();
      const targetId = route.targetRelayId.trim();
      const sourceName = routeSource.name || routeSource.id;
      if (!model || !targetId) return `供应商 ${sourceName} 的单模型路由需要填写匹配模型和目标供应商。`;
      if (seen.has(model)) return `供应商 ${sourceName} 的模型 ${model} 配置了重复路由。`;
      seen.add(model);
      if (targetId === routeSource.id) return `模型 ${model} 不能路由回当前供应商。`;
      const target = profiles.find((candidate) => candidate.id === targetId);
      if (!target) return `模型 ${model} 的目标供应商不存在。`;
      if (target.relayMode === "aggregate") return `模型 ${model} 不能路由到聚合供应商。`;
      if (target.protocol !== "responses") return `模型 ${model} 的目标供应商必须使用 Responses API。`;
      const usesOfficialOnly = target.relayMode === "official" && !target.officialMixApiKey;
      if (usesOfficialOnly || !target.baseUrl.trim() || !target.apiKey.trim()) {
        return `模型 ${model} 的目标供应商需要完整的 Base URL 和 Key。`;
      }
    }
  }
  return "";
}

export function hasCompleteModelRoutes(profile: RelayModelRouteProfile): boolean {
  return normalizeRelayModelRoutes(profile.modelRoutes).some(
    (route) => Boolean(route.model.trim() && route.targetRelayId.trim()),
  );
}
