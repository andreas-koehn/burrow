package api

// What is not converted yet. Every area task deletes its own block from each
// list; the last task sets openAPIConversionDone and the lists are empty.
// A list must not grow: an operation that is added to the API gets its
// operationId, its schemas and its table rows in the commit that adds it.

// openAPIConversionDone turns TestOpenAPI_NothingPending on. The verify task
// sets it to true once the three lists below are empty.
const openAPIConversionDone = true

// openAPIPendingOperationIDs lists the operations that have no operationId
// yet, as "METHOD path".
var openAPIPendingOperationIDs = []string{}

// openAPIPendingEmpty lists the operations that still reference the Empty
// schema in a request body or a response.
var openAPIPendingEmpty = []string{}

// openAPIPendingResponseContent lists the operations whose 200/201/202
// response declares no content yet.
var openAPIPendingResponseContent = []string{}
