const model = require("./model");

module.exports = function legacy() {
  return model.makeModel();
};
