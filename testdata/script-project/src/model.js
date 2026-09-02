export default class Model {
  static create() {
    return new Model();
  }
}

export function makeModel() {
  return Model.create();
}
