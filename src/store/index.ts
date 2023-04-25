import { OpenAIEmbeddings } from "langchain/embeddings/openai";
import { OpenAI } from "langchain/llms/openai";
import { inMemory } from "node-inmemory";
import { apiKey } from "../config";
import { BaseChain } from "langchain/chains";
import { Document } from "langchain/document";

export const [getEmbeddings] = inMemory(() => new OpenAIEmbeddings({
    openAIApiKey: apiKey,
}));

export const [getModel] = inMemory(() => new OpenAI({
    openAIApiKey: apiKey,
}));

export interface Store {
    saveVector(docs: Document[]): Promise<void>;
    getChain(): Promise<BaseChain>;
}

import PrismaStore from "./prisma";
import HNSWLibStore from "./hnswlib";

export { PrismaStore, HNSWLibStore };