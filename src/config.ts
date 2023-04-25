import { config } from "dotenv";

config();

const apiKey = process.env.OPENAI_API_KEY;
const chunkSize = parseInt(process.env.CHUNK_SIZE || '512');
const vectorStore = process.env.VECTOR_STORE || "hnswlib";
const handler = process.env.HANDLER || "web";

if (!apiKey) {
    throw new Error("No API key provided");
} else if (!vectorStore || !vectorStore.match(/^(prisma|hnswlib)$/)) {
    throw new Error("Invalid store provided");
} else if (!handler || !handler.match(/^(web|file)$/)) {
    throw new Error("Invalid handler provided");
}

export { apiKey, chunkSize, vectorStore, handler };