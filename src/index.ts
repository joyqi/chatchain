import { inMemory } from 'node-inmemory';
import { llm, vectorStore, handler } from './config';
import { Handler, WebHandler } from './handler';
import { Responder, OpenAIResponder } from './responder';
import { PrismaStore, HNSWLibStore } from './store';

const [getVectorStore] = inMemory(() => {
    switch (vectorStore) {
        case "prisma":
            return new PrismaStore();
        case "hnswlib":
            return new HNSWLibStore();
        default:
            throw new Error("Invalid vector store");
    }
});

function getLLMResponder(): Responder {
    const store = getVectorStore();

    switch (llm) {
        case "openai":
            return new OpenAIResponder(store);
        default:
            throw new Error("Invalid LLM");
    }
}

function getHandler(): Handler {
    const store = getVectorStore();
    const responder = getLLMResponder();

    switch (handler) {
        case "web":
            return new WebHandler(store, responder);
        default:
            throw new Error("Invalid prompt handler");
    }
}

(async () => {
    const handler = getHandler();
    await handler.handle();
})();