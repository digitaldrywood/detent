import * as Context from "effect/Context";
import type * as Effect from "effect/Effect";
import type { ConnectionAttemptError } from "../connection/model.ts";
export class SshEnvironmentGateway extends Context.Service<
  SshEnvironmentGateway,
  {
    disconnect: (target: {
      readonly host: string;
    }) => Effect.Effect<void, ConnectionAttemptError>;
  }
>()("SshEnvironmentGateway") {}
