import type { API } from "./api";
import React from "react";

export class Service {
  execute() {}
}

export const helper = () => true;

export function Widget() {
  return <section />;
}

export const Card = () => <div />;

export class Panel extends React.Component {
  render() {
    return <main />;
  }
}
