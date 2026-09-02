import { Service, helper } from "@app/service";
import Model, { makeModel } from "./model";
import * as esm from "./esm";
import React from "react";
export { makeModel } from "./model";
export * from "./types";

export interface API extends BaseAPI {
  run(): void;
}

export type ID = string;

export class Controller extends Service implements API {
  run() {
    helper();
    this.local();
    esm.esmFunction();
    Model.create();
  }

  local() {}
}

export function handle() {
  return helper();
}

const moduleName = "./legacy";
import(moduleName);

React.createElement("div");
runtime.dispatch();
