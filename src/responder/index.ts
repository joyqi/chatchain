import { BaseLLM } from "langchain/dist/llms/base";
import { chatLang } from "../config";
import { Store } from "../store";
import { ConversationalRetrievalQAChain } from "langchain/chains";

const qaTemplate = `Use the following pieces of context to answer the question ${chatLang ? `in ${chatLang}` : ''} at the end. If you don't know the answer, just say that you don't know, don't try to make up an answer.
{context}
Question: {question}
Helpful Answer:`;

export type Respond = (question: string, fn?: (str: string) => void) => Promise<string>;

export abstract class Responder {
    protected history: string[] = [];
    
    constructor(
        protected store: Store
    ) {}

    protected createLLMChain(model: BaseLLM) {
        return ConversationalRetrievalQAChain.fromLLM(model, this.store.getVectorStore(), { qaTemplate });
    }

    abstract respond(streaming: boolean): Respond;
}

import OpenAIResponder from "./openai";

export { OpenAIResponder };