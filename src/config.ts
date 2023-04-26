import { config } from "dotenv";

config();

const llm = process.env.LLM || "openai";
const llmKey = process.env.LLM_API_KEY;
const llmLang = process.env.LLM_LANG;
const chunkSize = parseInt(process.env.CHUNK_SIZE || '512');
const vectorStore = process.env.VECTOR_STORE || "hnswlib";
const handler = process.env.HANDLER || "web";

if (!llm || !llm.match(/^(openai)$/)) {
    throw new Error("Invalid LLM provided");
} else if (!llmKey) {
    throw new Error("No API key provided");
} else if (!vectorStore || !vectorStore.match(/^(prisma|hnswlib)$/)) {
    throw new Error("Invalid store provided");
} else if (!handler || !handler.match(/^(web|file)$/)) {
    throw new Error("Invalid handler provided");
}

export { llm, llmKey, chunkSize, llmLang, vectorStore, handler };